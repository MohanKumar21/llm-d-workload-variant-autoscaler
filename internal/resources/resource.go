/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"context"
	"fmt"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

const draDeviceClassResourcePrefix = "deviceclass.resource.kubernetes.io/"

// GetContainersGPUs returns the total GPU count across all containers
func GetContainersGPUs(containers []corev1.Container) int {
	total := 0
	for _, container := range containers {
		for _, resource := range constants.VendorResources {
			resName := corev1.ResourceName(resource.ResourceName)
			if qty, ok := container.Resources.Requests[resName]; ok {
				total += int(qty.Value())
			}
		}
	}
	return total
}

// GetContainersDRAExtendedResources returns the total device count requested
// through DRA's deviceclass.resource.kubernetes.io/<device-class> resource prefix.
func GetContainersDRAExtendedResources(containers []corev1.Container) int {
	total := 0
	for _, container := range containers {
		for name, qty := range container.Resources.Requests {
			if strings.HasPrefix(string(name), draDeviceClassResourcePrefix) {
				total += int(qty.Value())
			}
		}
	}
	return total
}

// GetPodTemplateGPUs returns the accelerator count declared by a pod template,
// including standard extended GPU resources and exact-count DRA claims.
func GetPodTemplateGPUs(ctx context.Context, c client.Reader, namespace string, template *corev1.PodTemplateSpec) (int, error) {
	if template == nil {
		return 0, nil
	}

	total := GetContainersGPUs(template.Spec.Containers)
	total += GetContainersDRAExtendedResources(template.Spec.Containers)

	draCount, err := GetPodSpecDRADeviceCount(ctx, c, namespace, &template.Spec)
	if err != nil {
		return total, err
	}
	total += draCount

	return total, nil
}

// GetPodDRADeviceCount returns the DRA device count for a concrete pod. When
// claims have already been generated and allocated, allocation status is used
// because it also covers allocationMode=All.
func GetPodDRADeviceCount(ctx context.Context, c client.Reader, pod *corev1.Pod) (int, error) {
	if pod == nil {
		return 0, nil
	}
	return getPodSpecDRADeviceCount(ctx, c, pod.Namespace, &pod.Spec, pod.Status.ResourceClaimStatuses)
}

// GetPodSpecDRADeviceCount returns the exact-count DRA device count declared by
// pod.spec.resourceClaims. Unsupported allocation modes contribute zero.
func GetPodSpecDRADeviceCount(ctx context.Context, c client.Reader, namespace string, spec *corev1.PodSpec) (int, error) {
	return getPodSpecDRADeviceCount(ctx, c, namespace, spec, nil)
}

func getPodSpecDRADeviceCount(ctx context.Context, c client.Reader, namespace string, spec *corev1.PodSpec, statuses []corev1.PodResourceClaimStatus) (int, error) {
	if spec == nil || len(spec.ResourceClaims) == 0 {
		return 0, nil
	}
	if c == nil {
		return 0, nil
	}

	statusByName := make(map[string]string, len(statuses))
	for _, status := range statuses {
		if status.ResourceClaimName != nil {
			statusByName[status.Name] = *status.ResourceClaimName
		}
	}

	total := 0
	for _, podClaim := range spec.ResourceClaims {
		switch {
		case podClaim.ResourceClaimName != nil:
			count, err := getResourceClaimDeviceCount(ctx, c, namespace, *podClaim.ResourceClaimName)
			if err != nil {
				return total, err
			}
			total += count
		case podClaim.ResourceClaimTemplateName != nil:
			if generatedName := statusByName[podClaim.Name]; generatedName != "" {
				count, err := getResourceClaimDeviceCount(ctx, c, namespace, generatedName)
				if err != nil {
					return total, err
				}
				total += count
				continue
			}

			count, err := getResourceClaimTemplateDeviceCount(ctx, c, namespace, *podClaim.ResourceClaimTemplateName)
			if err != nil {
				return total, err
			}
			total += count
		}
	}
	return total, nil
}

func getResourceClaimDeviceCount(ctx context.Context, c client.Reader, namespace, name string) (int, error) {
	claim := &resourcev1.ResourceClaim{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, claim); err != nil {
		if meta.IsNoMatchError(err) {
			return 0, nil
		}
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get ResourceClaim %s/%s: %w", namespace, name, err)
	}
	if claim.Status.Allocation != nil {
		allocated := len(claim.Status.Allocation.Devices.Results)
		if allocated > 0 {
			return allocated, nil
		}
	}
	return CountDRADeviceRequests(claim.Spec.Devices.Requests), nil
}

func getResourceClaimTemplateDeviceCount(ctx context.Context, c client.Reader, namespace, name string) (int, error) {
	template := &resourcev1.ResourceClaimTemplate{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, template); err != nil {
		if meta.IsNoMatchError(err) {
			return 0, nil
		}
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get ResourceClaimTemplate %s/%s: %w", namespace, name, err)
	}
	return CountDRADeviceRequests(template.Spec.Spec.Devices.Requests), nil
}

// CountDRADeviceRequests returns the count for DRA requests that declare an
// exact count. The Kubernetes default for ExactCount with omitted count is one.
func CountDRADeviceRequests(requests []resourcev1.DeviceRequest) int {
	total := 0
	for _, request := range requests {
		if request.Exactly != nil {
			total += exactDeviceRequestCount(request.Exactly.AllocationMode, request.Exactly.Count)
			continue
		}
		if len(request.FirstAvailable) > 0 {
			total += firstAvailableDeviceRequestCount(request.FirstAvailable)
		}
	}
	return total
}

func firstAvailableDeviceRequestCount(requests []resourcev1.DeviceSubRequest) int {
	maxCount := 0
	for _, request := range requests {
		count := exactDeviceRequestCount(request.AllocationMode, request.Count)
		if count > maxCount {
			maxCount = count
		}
	}
	return maxCount
}

func exactDeviceRequestCount(mode resourcev1.DeviceAllocationMode, count int64) int {
	switch mode {
	case "", resourcev1.DeviceAllocationModeExactCount:
		if count <= 0 {
			return 1
		}
		return int(count)
	default:
		return 0
	}
}

// GetResourceWithBackoff performs a Get operation with exponential backoff retry logic
func GetResourceWithBackoff[T client.Object](ctx context.Context, c client.Client, objKey client.ObjectKey, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, objKey, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err // Don't retry on notFound errors
			}

			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Error(err, "transient error getting resource, retrying",
				"resourceType", resourceType,
				"name", objKey.Name,
				"namespace", objKey.Namespace)
			return false, nil // Retry on transient errors
		}

		return true, nil
	})
}
