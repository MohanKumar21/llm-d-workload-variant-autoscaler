package discovery

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func draUsageNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-nvidia",
			Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB"},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
		},
	}
}

func draUsagePod(name, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "node-nvidia",
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To(claimName),
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func draGPUClaim(name string, count int64) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name:    "gpu",
					Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "gpu.example.com", Count: count},
				}},
			},
		},
	}
}

func draUsageScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	return scheme
}

func TestDiscoverUsage_DRAClaimCounted(t *testing.T) {
	scheme := draUsageScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		draUsageNode(),
		draUsagePod("pod-1", "gpu-claim"),
		draGPUClaim("gpu-claim", 3),
	).Build()

	result, err := NewK8sWithGpuOperator(c).DiscoverUsage(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 3, result["NVIDIA-H100-SXM5-80GB"])
}

func TestDiscoverUsage_SharedDRAClaimCountedOnce(t *testing.T) {
	scheme := draUsageScheme(t)
	// Two pods reference the same named claim; its devices count once.
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		draUsageNode(),
		draUsagePod("pod-1", "shared-claim"),
		draUsagePod("pod-2", "shared-claim"),
		draGPUClaim("shared-claim", 2),
	).Build()

	result, err := NewK8sWithGpuOperator(c).DiscoverUsage(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 2, result["NVIDIA-H100-SXM5-80GB"])
}

func TestDiscoverUsage_DRAErrorDegradesPerPod(t *testing.T) {
	scheme := draUsageScheme(t)
	// The claim Get fails, but usage discovery must not error out the whole cycle;
	// the pod's DRA contribution is skipped and the result is empty (no non-DRA GPUs).
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(draUsageNode(), draUsagePod("pod-1", "gpu-claim")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*resourcev1.ResourceClaim); ok {
					return apierrors.NewForbidden(schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, key.Name, errors.New("rbac not propagated"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	result, err := NewK8sWithGpuOperator(c).DiscoverUsage(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 0, result["NVIDIA-H100-SXM5-80GB"])
}
