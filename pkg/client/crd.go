// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	logr "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultPollInterval is the default interval for polling CRD status
	defaultPollInterval = 150 * time.Millisecond
	// DefaultTimeout is the default timeout for waiting for CRD status
	defaultTimeout = 2 * time.Minute
)

var _ CRDClient = &CRDWrapper{}

// CRDClient represents operations for managing CustomResourceDefinitions
type CRDClient interface {
	// EnsureCreated ensures a CRD exists and is ready
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name, version string) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)
}

// CRDWrapper provides a simplified interface for CRD operations
type CRDWrapper struct {
	client       apiextensionsv1.CustomResourceDefinitionInterface
	pollInterval time.Duration
	timeout      time.Duration
}

// CRDWrapperConfig contains configuration for the CRD wrapper
type CRDWrapperConfig struct {
	Client       *apiextensionsv1.ApiextensionsV1Client
	PollInterval time.Duration
	Timeout      time.Duration
}

// DefaultConfig returns a CRDWrapperConfig with default values
func DefaultCRDWrapperConfig() CRDWrapperConfig {
	return CRDWrapperConfig{
		PollInterval: defaultPollInterval,
		Timeout:      defaultTimeout,
	}
}

// newCRDWrapper creates a new CRD wrapper
func newCRDWrapper(cfg CRDWrapperConfig) *CRDWrapper {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}

	return &CRDWrapper{
		client:       cfg.Client.CustomResourceDefinitions(),
		pollInterval: cfg.PollInterval,
		timeout:      cfg.Timeout,
	}
}

// Ensure ensures a CRD exists, up-to-date, and is ready. This can be
// a dangerous operation as it will update the CRD if it already exists.
//
// The caller is responsible for ensuring the CRD, isn't introducing
// breaking changes.
func (w *CRDWrapper) Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error {
	log := logr.FromContext(ctx)
	existingCRD, err := w.Get(ctx, crd.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing CRD: %w", err)
		}

		log.Info("Creating CRD", "name", crd.Name)
		if err := w.create(ctx, crd); err != nil {
			return fmt.Errorf("failed to create CRD: %w", err)
		}
	} else {
		// Sanity check
		if len(crd.Spec.Versions) != 1 {
			return fmt.Errorf("expected only one crdVersion, got %d", len(crd.Spec.Versions))
		}

		// Check if new API crdVersion is introduced or if the CRD has changed
		var prioVersion string
		versionExists := false
		for i, crdVersion := range existingCRD.Spec.Versions {
			// We currently only support promoting a crdVersion using the "None" conversion strategy. Thus, we enforce
			// an equal schema for all versions.
			if !equality.Semantic.DeepEqual(crdVersion.Schema, crd.Spec.Versions[0].Schema) {
				return fmt.Errorf("CRD schema differs between versions which is currently not supported")
			}

			if crdVersion.Name == crd.Spec.Versions[0].Name {
				if crdVersion.Served {
					log.Info("CRD crdVersion already exists and is up-to-date", "name", crd.Name, "crdVersion", crdVersion.Name)
					return nil
				}

				log.Info("CRD crdVersion already exists but is not served, updating", "name", crd.Name, "crdVersion", crdVersion.Name)
				versionExists = true
				existingCRD.Spec.Versions[i] = crd.Spec.Versions[0]
			}

			// Find the highest priority version to set it as storage version (see below)
			// CompareKubeAwareVersionStrings returns >0 if first param is greater than the second one
			if version.CompareKubeAwareVersionStrings(crdVersion.Name, prioVersion) > 0 {
				prioVersion = crdVersion.Name
			}
		}

		if version.CompareKubeAwareVersionStrings(crd.Spec.Versions[0].Name, prioVersion) > 0 {
			prioVersion = crd.Spec.Versions[0].Name
		}

		// Append new version if it is not already present
		if !versionExists {
			existingCRD.Spec.Versions = append(existingCRD.Spec.Versions, crd.Spec.Versions[0])
		}

		// Loop over every existing version and set the storage version to the one with the highest priority.
		for i, crdVersion := range existingCRD.Spec.Versions {
			if crdVersion.Name == prioVersion {
				log.Info("Setting CRD version as storage", "name", crd.Name, "crdVersion", existingCRD.Spec.Versions[i].Name)
				existingCRD.Spec.Versions[i].Storage = true
			} else {
				existingCRD.Spec.Versions[i].Storage = false
			}
		}

		log.Info("Set conversion strategy to 'None'", "name", crd.Name)
		existingCRD.Spec.Conversion = &v1.CustomResourceConversion{
			Strategy: v1.NoneConverter,
		}

		log.Info("Adding new CRD crdVersion", "name", crd.Name, "crdVersion", crd.Spec.Versions[0].Name)
		if err := w.patch(ctx, *existingCRD); err != nil {
			return fmt.Errorf("failed to patch CRD: %w", err)
		}
	}

	return w.waitForReady(ctx, crd.Name)
}

// Get retrieves a CRD by name
func (w *CRDWrapper) Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	return w.client.Get(ctx, name, metav1.GetOptions{})
}

func (w *CRDWrapper) create(ctx context.Context, crd v1.CustomResourceDefinition) error {
	_, err := w.client.Create(ctx, &crd, metav1.CreateOptions{})
	return err
}

func (w *CRDWrapper) patch(ctx context.Context, newCRD v1.CustomResourceDefinition) error {
	patchBytes, err := json.Marshal(newCRD)
	if err != nil {
		return fmt.Errorf("failed to marshal CRD for patch: %w", err)
	}

	_, err = w.client.Patch(
		ctx,
		newCRD.Name,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	return err
}

// Delete removes a CRD if it exists
func (w *CRDWrapper) Delete(ctx context.Context, name, APIversion string) error {
	log := logr.FromContext(ctx)

	existingCRD, err := w.Get(ctx, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("CRD not found, nothing to delete", "name", name)
			return nil
		}
		return fmt.Errorf("failed to get CRD %s/%s: %w", APIversion, name, err)
	}

	if len(existingCRD.Spec.Versions) > 1 {
		log.Info("CRD has multiple versions, setting passed version to unserved", "name", name, "version", APIversion)

		// Check if that version is set as storage version to reset it to the one with the highest priority
		var prioVersion string
		for i, crdVersion := range existingCRD.Spec.Versions {
			// Check for the version that is set as new storage version. Must be one of the served versions.
			if crdVersion.Name != APIversion {
				if version.CompareKubeAwareVersionStrings(crdVersion.Name, prioVersion) > 0 && crdVersion.Served {
					prioVersion = crdVersion.Name
				}
				continue
			}

			// Make sure that the version is not set as stored or served version
			// This is currently possible, because we only support the conversion strategy "None". So, we can choose any
			// other version as storage version.
			existingCRD.Spec.Versions[i].Served = false
			existingCRD.Spec.Versions[i].Storage = false
		}

		// If no served and highest-priority version was found, we can delete the CRD as it is not used anymore
		if prioVersion == "" {
			log.Info("No served version found, deleting CRD", "name", name)
			err := w.client.Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete CRD: %w", err)
			}
			return nil
		}

		// Otherwise, set the served version with the highest priority as storage version and update the CRD.
		for i, crdVersion := range existingCRD.Spec.Versions {
			if crdVersion.Name == prioVersion {
				log.Info("Setting new storage version", "name", name, "version", prioVersion)
				existingCRD.Spec.Versions[i].Storage = true
			} else {
				existingCRD.Spec.Versions[i].Storage = false
			}
		}

		log.Info("Removing CRD version", "name", name, "version", APIversion)
		err := w.patch(ctx, *existingCRD)
		if err != nil {
			return fmt.Errorf("failed to patch CRD to remove version %s: %w", APIversion, err)
		}
	} else {
		log.Info("Deleting CRD", "name", name)
		err := w.client.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete CRD: %w", err)
		}
	}

	return nil
}

// waitForReady waits for a CRD to become ready
func (w *CRDWrapper) waitForReady(ctx context.Context, name string) error {
	log := logr.FromContext(ctx)
	log.Info("Waiting for CRD to become ready", "name", name)

	return wait.PollUntilContextTimeout(ctx, w.pollInterval, w.timeout, true,
		func(ctx context.Context) (bool, error) {
			crd, err := w.Get(ctx, name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}

			for _, cond := range crd.Status.Conditions {
				if cond.Type == v1.Established && cond.Status == v1.ConditionTrue {
					return true, nil
				}
			}

			return false, nil
		})
}
