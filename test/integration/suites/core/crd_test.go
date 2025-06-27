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

package core_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("CRD", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("CRD Creation", func() {
		It("should create CRD when ResourceGraphDefinition is created", func() {
			// Create a simple ResourceGraphDefinition
			rgd := generator.NewResourceGraphDefinition("test-crd",
				generator.WithSchema(
					"TestResource", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
				generator.WithResource("res1", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.field1}",
					},
					"data": map[string]interface{}{
						"key":  "value",
						"key2": "${schema.spec.field2}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// Verify CRD is created
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: "testresources.kro.run",
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// Verify CRD spec
				g.Expect(crd.Spec.Group).To(Equal("kro.run"))
				g.Expect(crd.Spec.Names.Kind).To(Equal("TestResource"))
				g.Expect(crd.Spec.Names.Plural).To(Equal("testresources"))

				// Verify schema
				props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties

				// Check spec schema
				g.Expect(props["spec"].Properties["field1"].Type).To(Equal("string"))
				g.Expect(props["spec"].Properties["field2"].Type).To(Equal("integer"))
				g.Expect(props["spec"].Properties["field2"].Default.Raw).To(Equal([]byte("42")))
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should delete CRD when ResourceGraphDefinition is deleted", func() {
			// Create ResourceGraphDefinition
			rgd := generator.NewResourceGraphDefinition("test-crd-delete",
				generator.WithSchema(
					"TestDelete", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// Wait for CRD creation
			crdName := "testdeletes.kro.run"
			Eventually(func() error {
				return env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
			}, 10*time.Second, time.Second).Should(Succeed())

			// Delete ResourceGraphDefinition
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

			// Verify CRD is deleted
			Eventually(func() bool {
				err := env.Client.Get(ctx, types.NamespacedName{Name: crdName},
					&apiextensionsv1.CustomResourceDefinition{})
				return errors.IsNotFound(err)
			}, 10*time.Second, time.Second).Should(BeTrue())
		})
	})

	Context("CRD Watch Reconciliation", func() {
		It("should update the ResourceGraphDefinition with an error when the CRD is manually modified", func() {
			rgdName := "test-crd-watch"
			rgd := generator.NewResourceGraphDefinition(rgdName,
				generator.WithSchema(
					"TestWatch", "v1alpha1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42",
					},
					nil,
				),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			// wait for CRD to be created and verify its initial state
			crdName := "testwatches.kro.run"
			crd := &apiextensionsv1.CustomResourceDefinition{}

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(metadata.IsKROOwned(crd.ObjectMeta)).To(BeTrue())
				g.Expect(crd.Labels[metadata.ResourceGraphDefinitionNameLabel]).To(Equal(rgdName))

				// store the original schema for later comparison
				originalSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
				g.Expect(originalSchema["field1"].Type).To(Equal("string"))
				g.Expect(originalSchema["field2"].Type).To(Equal("integer"))
				g.Expect(originalSchema["field2"].Default.Raw).To(Equal([]byte("42")))
			}, 10*time.Second, time.Second).Should(Succeed())

			// Manually modify the CRD to simulate external modification
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: crdName,
				}, crd)
				g.Expect(err).ToNot(HaveOccurred())

				// modify the schema (removing field2)
				delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "field2")

				err = env.Client.Update(ctx, crd)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).Should(Succeed())

			rgd = &krov1alpha1.ResourceGraphDefinition{}

			// Verify that the reconciliation updates the ResourceGraphDefinition status with an error
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgdName,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())

				c := krov1alpha1.GetCondition(rgd.Status.Conditions, krov1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced)
				g.Expect(c).ToNot(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
				// TODO: Consider using constant
				g.Expect(*c.Reason).To(Equal("CRD schema differs between versions which is currently not supported"))

			}, 20*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("CRD API Versioning", func() {
		It("should add new CRD API versions", func(ctx SpecContext) {
			kind := "TestVersioning"
			versions := []string{"v1alpha1", "v1alpha2", "v1beta1", "v1", "v2alpha1", "v2"}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			for _, version := range versions {
				// Create a simple ResourceGraphDefinition
				rgd := generator.NewResourceGraphDefinition("test-crd-"+version,
					generator.WithSchema(kind, version, map[string]interface{}{"field1": "string"}, nil),
				)
				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				// Verify CRD is created
				Eventually(func(ctx context.Context) error {
					if err := env.Client.Get(ctx, types.NamespacedName{Name: strings.ToLower(kind) + "s.kro.run"}, crd); err != nil {
						return fmt.Errorf("failed to get CRD %s: %w", kind, err)
					}

					for _, v := range crd.Spec.Versions {
						if v.Name == version {
							return nil
						}
					}

					return fmt.Errorf("CRD %s has no version %s", kind, version)
				}, 30*time.Second).WithContext(ctx).Should(Succeed())
			}

			// Verify that all versions are present in the CRD
			Expect(len(versions)).To(Equal(len(crd.Spec.Versions)), fmt.Sprintf("Expected %d versions, got %d", len(versions), len(crd.Spec.Versions)))

			// Verify that the latest version is set as storage
			for _, v := range crd.Spec.Versions {
				if v.Name == versions[len(versions)-1] {
					Expect(v.Storage).To(BeTrue(), fmt.Sprintf("Expected version %s to be set as storage", v.Name))
				}
			}
		})

		It("should error when a new CRD API version has a different schema", func(ctx SpecContext) {
			kind := "TestVersioningSchemaMismatch"
			rgd1 := generator.NewResourceGraphDefinition("test-crd-schema-v1alpha1",
				generator.WithSchema(kind, "v1alpha1", map[string]interface{}{"field1": "string"}, nil),
			)
			Expect(env.Client.Create(ctx, rgd1)).To(Succeed())

			// Verify CRD is created
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Eventually(func(ctx context.Context) error {
				return env.Client.Get(ctx, types.NamespacedName{Name: strings.ToLower(kind) + "es.kro.run"}, crd)
			}, 30*time.Second).WithContext(ctx).Should(Succeed())

			rgd2 := generator.NewResourceGraphDefinition("test-crd-schema-v1",
				generator.WithSchema(
					kind, "v1",
					map[string]interface{}{
						"field1": "string",
						"field2": "integer | default=42", // Different schema from v1alpha1
					},
					nil,
				),
			)
			Expect(env.Client.Create(ctx, rgd2)).To(Succeed())

			// Verify that the RGD reconciliation fails due to schema mismatch
			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: rgd2.Name}, rgd2); err != nil {
					return fmt.Errorf("failed to get RGD %s: %w", rgd2.Name, err)
				}

				c := krov1alpha1.GetCondition(rgd2.Status.Conditions, krov1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced)
				if c == nil {
					return fmt.Errorf("condition %s not found in RGD %s", krov1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced, rgd2.Name)
				}

				if c.Status != metav1.ConditionFalse {
					return fmt.Errorf("expected condition status to be False, got %s", c.Status)
				}

				return nil
			}, 30*time.Second).WithContext(ctx).Should(Succeed())

			// Verify that the CRD has only one version
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: strings.ToLower(kind) + "es.kro.run"}, crd)).To(Succeed())
			Expect(len(crd.Spec.Versions)).To(Equal(1), fmt.Sprintf("Expected CRD %s to have 1 version, got %d", kind, len(crd.Spec.Versions)))
		})

		It("should remove a CRD version if the respective RGD is deleted", func(ctx SpecContext) {
			// We will create multiple versions of a CRD and then delete the last one. We expect the CRD to set the
			// last version to unserved and update the storage version to the highest priority served version that is
			// available.
			// Afterwards, we will delete the first version and expect the CRD only to have this version unset, while
			// the storage version remains the same.
			// Finally, we will delete the remaining versions and expect the CRD to be deleted.

			kind := "TestVersioningDeletion"
			versions := []string{"v1alpha1", "v1beta1", "v1", "v2"}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			crdName := strings.ToLower(kind) + "s.kro.run"
			for _, version := range versions {
				// Create a simple ResourceGraphDefinition
				rgd := generator.NewResourceGraphDefinition("test-crd-deletion-"+version,
					generator.WithSchema(kind, version, map[string]interface{}{"field1": "string"}, nil),
				)
				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				// Verify CRD is created
				Eventually(func(ctx context.Context) error {
					if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
						return fmt.Errorf("failed to get CRD %s: %w", kind, err)
					}

					for _, v := range crd.Spec.Versions {
						if v.Name == version {
							return nil
						}
					}

					return fmt.Errorf("CRD %s has no version %s", kind, version)
				}, 30*time.Second).WithContext(ctx).Should(Succeed())
			}

			// Delete the last ResourceGraphDefinition
			Expect(env.Client.Delete(ctx, &krov1alpha1.ResourceGraphDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-crd-deletion-" + versions[len(versions)-1],
				},
			})).To(Succeed())

			versionsUpdated := versions[:len(versions)-1]

			// Verify that the CRD version is set to unserved and the storage version is updated
			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
					return fmt.Errorf("failed to get CRD %s: %w", kind, err)
				}

				for _, v := range crd.Spec.Versions {
					switch v.Name {
					// Check the last version that was deleted
					case versions[len(versions)-1]:
						if v.Storage {
							return fmt.Errorf("expected version %s to not be set as storage", v.Name)
						}

						if v.Served {
							return fmt.Errorf("expected version %s to not be set as served", v.Name)
						}

					// Check the new storage version
					case versionsUpdated[len(versionsUpdated)-1]:
						if !v.Served {
							return fmt.Errorf("expected version %s to be set as served", v.Name)
						}
					// All other versions should still be served but not set as storage
					default:
						if !v.Served {
							return fmt.Errorf("expected version %s to be set as served", v.Name)
						}

						if v.Storage {
							return fmt.Errorf("expected version %s to not be set as storage", v.Name)
						}
					}
				}

				return nil
			}, 30*time.Second).WithContext(ctx).Should(Succeed())

			// Delete the first ResourceGraphDefinition
			Expect(env.Client.Delete(ctx, &krov1alpha1.ResourceGraphDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-crd-deletion-" + versions[0],
				},
			})).To(Succeed())

			// Verify that the first CRD version is set to unserved and the storage version remains the same
			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
					return fmt.Errorf("failed to get CRD %s: %w", kind, err)
				}

				for _, v := range crd.Spec.Versions {
					switch v.Name {
					// First version that was deleted, should not be served anymore
					case versions[0]:
						if v.Served {
							return fmt.Errorf("expected version %s to not be set as served", v.Name)
						}
					// Last version that was deleted, should not be served anymore
					case versions[len(versions)-1]:
						if v.Served {
							return fmt.Errorf("expected version %s to not be set as served", v.Name)
						}
						if v.Storage {
							return fmt.Errorf("expected version %s to not be set as storage", v.Name)
						}
					// Version with the highest version priority should still be set as storage
					case versionsUpdated[len(versionsUpdated)-1]:
						if !v.Storage {
							return fmt.Errorf("expected version %s to be set as served", v.Name)
						}
					// All other versions should still be served but not set as storage
					default:
						if !v.Served {
							return fmt.Errorf("expected version %s to be set as served", v.Name)
						}

						if v.Storage {
							return fmt.Errorf("expected version %s to not be set as storage", v.Name)
						}
					}
				}

				return nil
			}, 30*time.Second).WithContext(ctx).Should(Succeed())

			// Delete the remaining ResourceGraphDefinitions and verify the CRD is deleted
			for _, version := range versionsUpdated[1:] {
				Expect(env.Client.Delete(ctx, &krov1alpha1.ResourceGraphDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-crd-deletion-" + version,
					},
				})).To(Succeed())
			}

			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
					if errors.IsNotFound(err) {
						return nil // CRD is deleted
					}

					return fmt.Errorf("failed to get CRD %s: %w", kind, err)
				}

				return fmt.Errorf("CRD %s still exists", kind)
			}, 30*time.Second).WithContext(ctx).Should(Succeed())
		})

		It("should re-add a CRD version that was removed", func(ctx SpecContext) {
			kind := "TestVersioningReAdd"
			versions := []string{"v1alpha1", "v1beta1", "v1"}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			crdName := strings.ToLower(kind) + "s.kro.run"
			for _, version := range versions {
				// Create a simple ResourceGraphDefinition
				rgd := generator.NewResourceGraphDefinition("test-crd-re-add-"+version,
					generator.WithSchema(kind, version, map[string]interface{}{"field1": "string"}, nil),
				)
				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				// Verify CRD is created
				Eventually(func(ctx context.Context) error {
					if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
						return fmt.Errorf("failed to get CRD %s: %w", kind, err)
					}

					for _, v := range crd.Spec.Versions {
						if v.Name == version {
							return nil
						}
					}

					return fmt.Errorf("CRD %s has no version %s", kind, version)
				}, 30*time.Second).WithContext(ctx).Should(Succeed())
			}

			// Delete the last ResourceGraphDefinition
			lastVersion := versions[len(versions)-1]
			Expect(env.Client.Delete(ctx, &krov1alpha1.ResourceGraphDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-crd-re-add-" + lastVersion},
			})).To(Succeed())

			// Wait for the CRD version to be removed
			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
					return fmt.Errorf("failed to get CRD %s: %w", kind, err)
				}

				for _, v := range crd.Spec.Versions {
					if v.Name == lastVersion && v.Served {
						return fmt.Errorf("CRD %s still serves version %s", kind, lastVersion)
					}
				}

				// Ensure the RGD is deleted
				if err := env.Client.Get(ctx, types.NamespacedName{Name: "test-crd-re-add-" + lastVersion}, &krov1alpha1.ResourceGraphDefinition{}); err != nil {
					if !errors.IsNotFound(err) {
						return fmt.Errorf("failed to get RGD test-crd-re-add-%s: %w", lastVersion, err)
					}
				} else {
					return fmt.Errorf("RGD test-crd-re-add-%s still exists after deletion", lastVersion)
				}

				return nil
			}, 30*time.Second).WithContext(ctx).Should(Succeed())

			// Now re-add the version
			rgdReAdd := generator.NewResourceGraphDefinition("test-crd-re-add-"+lastVersion,
				generator.WithSchema(kind, lastVersion, map[string]interface{}{"field1": "string"}, nil),
			)
			Expect(env.Client.Create(ctx, rgdReAdd)).To(Succeed())

			// Verify that the CRD version is re-added and the storage version is updated
			Eventually(func(ctx context.Context) error {
				if err := env.Client.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
					return fmt.Errorf("failed to get CRD %s: %w", kind, err)
				}

				for _, v := range crd.Spec.Versions {
					if v.Name == lastVersion {
						if v.Storage {
							return nil
						}
						return fmt.Errorf("CRD %s has version %s but it is not set as storage", kind, lastVersion)
					}
				}

				return fmt.Errorf("CRD %s does not have version %s after re-adding", kind, lastVersion)
			}, 30*time.Second).WithContext(ctx).Should(Succeed())
		})
	})
})
