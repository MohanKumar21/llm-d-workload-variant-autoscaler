package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGetPodTemplateGPUsResourceClaimTemplate(t *testing.T) {
	ctx := context.Background()
	c := newResourceClient(t, &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-template",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{{
						Name: "gpu",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "gpu.example.com",
							Count:           2,
						},
					}},
				},
			},
		},
	})
	template := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      "gpu",
				ResourceClaimTemplateName: ptr.To("gpu-template"),
			}},
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Claims: []corev1.ResourceClaim{{Name: "gpu"}},
				},
			}},
		},
	}

	count, err := GetPodTemplateGPUs(ctx, c, "default", template)

	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestGetPodTemplateGPUsDRAExtendedResourcePrefix(t *testing.T) {
	template := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName("deviceclass.resource.kubernetes.io/gpu.example.com"): resource.MustParse("3"),
					},
				},
			}},
		},
	}

	count, err := GetPodTemplateGPUs(context.Background(), newResourceClient(t), "default", template)

	require.NoError(t, err)
	require.Equal(t, 3, count)
}

func TestGetPodDRADeviceCountUsesAllocatedClaimStatus(t *testing.T) {
	ctx := context.Background()
	c := newResourceClient(t, &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-gpu-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: "gpu",
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: "gpu.example.com",
						Count:           1,
					},
				}},
			},
		},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Request: "gpu", Driver: "dra.example.com", Pool: "pool-a", Device: "gpu-0"},
						{Request: "gpu", Driver: "dra.example.com", Pool: "pool-a", Device: "gpu-1"},
						{Request: "gpu", Driver: "dra.example.com", Pool: "pool-a", Device: "gpu-2"},
					},
				},
			},
		},
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      "gpu",
				ResourceClaimTemplateName: ptr.To("gpu-template"),
			}},
		},
		Status: corev1.PodStatus{
			ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("pod-gpu-claim"),
			}},
		},
	}

	count, err := GetPodDRADeviceCount(ctx, c, pod, nil)

	require.NoError(t, err)
	require.Equal(t, 3, count)
}

func gpuResourceClaim(name string, count int64) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: "gpu",
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: "gpu.example.com",
						Count:           count,
					},
				}},
			},
		},
	}
}

func podWithNamedClaim(podName, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To(claimName),
			}},
		},
	}
}

func TestGetPodDRADeviceCountNamedClaim(t *testing.T) {
	c := newResourceClient(t, gpuResourceClaim("shared-gpu-claim", 2))
	pod := podWithNamedClaim("pod", "shared-gpu-claim")

	count, err := GetPodDRADeviceCount(context.Background(), c, pod, nil)

	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestGetPodDRADeviceCountDedupesSharedNamedClaim(t *testing.T) {
	c := newResourceClient(t, gpuResourceClaim("shared-gpu-claim", 2))
	seen := map[string]bool{}

	// Two pods reference the same named claim: it counts once, not twice.
	first, err := GetPodDRADeviceCount(context.Background(), c, podWithNamedClaim("pod-a", "shared-gpu-claim"), seen)
	require.NoError(t, err)
	require.Equal(t, 2, first)

	second, err := GetPodDRADeviceCount(context.Background(), c, podWithNamedClaim("pod-b", "shared-gpu-claim"), seen)
	require.NoError(t, err)
	require.Equal(t, 0, second)
}

func TestGetPodDRADeviceCountNotFoundIsZero(t *testing.T) {
	// Claim referenced but absent: treated as zero, not an error.
	c := newResourceClient(t)
	pod := podWithNamedClaim("pod", "missing-claim")

	count, err := GetPodDRADeviceCount(context.Background(), c, pod, nil)

	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestGetPodDRADeviceCountNoMatchIsZero(t *testing.T) {
	// resource.k8s.io CRD not served (no REST mapping) -> NoMatchError -> zero.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims"}}
		},
	}).Build()
	pod := podWithNamedClaim("pod", "some-claim")

	count, err := GetPodDRADeviceCount(context.Background(), c, pod, nil)

	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestGetPodDRADeviceCountPropagatesOtherErrors(t *testing.T) {
	// A non-NotFound Get error is wrapped and propagated.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "some-claim", errors.New("forbidden"))
		},
	}).Build()
	pod := podWithNamedClaim("pod", "some-claim")

	_, err := GetPodDRADeviceCount(context.Background(), c, pod, nil)

	require.Error(t, err)
	require.True(t, apierrors.IsForbidden(err))
}

func TestCountDRADeviceRequests(t *testing.T) {
	tests := []struct {
		name     string
		requests []resourcev1.DeviceRequest
		want     int
	}{
		{
			name: "exact count defaults to one",
			requests: []resourcev1.DeviceRequest{{
				Name: "gpu",
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu.example.com",
				},
			}},
			want: 1,
		},
		{
			name: "exact count uses requested count",
			requests: []resourcev1.DeviceRequest{{
				Name: "gpu",
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu.example.com",
					Count:           4,
				},
			}},
			want: 4,
		},
		{
			name: "all mode cannot be inferred before allocation",
			requests: []resourcev1.DeviceRequest{{
				Name: "gpu",
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu.example.com",
					AllocationMode:  resourcev1.DeviceAllocationModeAll,
				},
			}},
			want: 0,
		},
		{
			name: "first available uses largest exact subrequest",
			requests: []resourcev1.DeviceRequest{{
				Name: "gpu",
				FirstAvailable: []resourcev1.DeviceSubRequest{
					{
						Name:            "small",
						DeviceClassName: "gpu.example.com",
						Count:           1,
					},
					{
						Name:            "large",
						DeviceClassName: "gpu.example.com",
						Count:           3,
					},
				},
			}},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, CountDRADeviceRequests(tt.requests))
		})
	}
}

func newResourceClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
