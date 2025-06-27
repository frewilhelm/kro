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

// Ensure ensures a CRD exists, up-to-date, and is ready.
// It will create the CRD if it does not exist, or add the new API version if it matches the schema of an existing
// version.
// Currently, it only supports promoting an API version using the "None" conversion strategy, which means all versions
// must have the same schema.
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

		// Check if new API version is introduced or if the CRD has changed
		update := false
		for i, crdVersion := range existingCRD.Spec.Versions {
			// We currently only support promoting an API version using the "None" conversion strategy. Thus, we enforce
			// an equal schema for all versions.
			if !equality.Semantic.DeepEqual(crdVersion.Schema, crd.Spec.Versions[0].Schema) {
				return fmt.Errorf("CRD schema differs between versions which is currently not supported")
			}

			// We will just set the storage version to the new or re-added version. Thus, we set any other version to
			// false.
			existingCRD.Spec.Versions[i].Storage = false

			if crdVersion.Name == crd.Spec.Versions[0].Name {
				if crdVersion.Served {
					log.Info("CRD version already exists and is up-to-date", "name", crd.Name, "version", crdVersion.Name)
					return nil
				}

				log.Info("CRD version already exists but is not served, updating", "name", crd.Name, "version", crdVersion.Name)
				existingCRD.Spec.Versions[i] = crd.Spec.Versions[0]
				update = true
			} else {
				if !update && i == len(existingCRD.Spec.Versions)-1 {
					log.Info("Adding new CRD crdVersion", "name", crd.Name, "crdVersion", crd.Spec.Versions[0].Name)
					existingCRD.Spec.Versions = append(existingCRD.Spec.Versions, crd.Spec.Versions[0])
				}
			}
		}

		log.Info("Set conversion strategy to 'None'", "name", crd.Name)
		existingCRD.Spec.Conversion = &v1.CustomResourceConversion{
			Strategy: v1.NoneConverter,
		}

		log.Info("Patching existing CRD", "name", crd.Name)
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

// Delete removes a CRD if it exists and handles the case where the CRD has multiple versions.
// If the CRD has multiple versions, it will set the passed version to unserved. If all versions are unserved,
// it will delete the CRD.
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

		// Check if that version is set as storage version to reset it to any other served version. This is safe,
		// because we only support the "None" conversion strategy. So, we can choose any version as storage version.
		idxAnyServedVersion := -1
		isStorage := false
		for i, crdVersion := range existingCRD.Spec.Versions {
			if crdVersion.Name != APIversion {
				// Get index of any served version to set it as storage version later
				if idxAnyServedVersion == -1 && crdVersion.Served {
					idxAnyServedVersion = i
				}

				// Ignore any other version than the one we want to delete
				continue
			}

			isStorage = crdVersion.Storage

			// Make sure that the version to delete is not set as stored or served version
			existingCRD.Spec.Versions[i].Served = false
			existingCRD.Spec.Versions[i].Storage = false
		}

		// If no served version is found, we can delete the CRD as it is not used anymore
		if idxAnyServedVersion == -1 {
			log.Info("No served version found, deleting CRD", "name", name)
			err := w.client.Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete CRD: %w", err)
			}
			return nil
		}

		// If the passed version was set as storage, set any served version as storage version.
		if isStorage {
			log.Info("Setting another served version as storage version", "name", name, "version", existingCRD.Spec.Versions[idxAnyServedVersion].Name)
			existingCRD.Spec.Versions[idxAnyServedVersion].Storage = true
		}

		log.Info("Setting CRD version to unserved", "name", name, "version", APIversion)
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
