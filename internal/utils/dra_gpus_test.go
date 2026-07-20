package utils

import (
	"context"
	"errors"
	"testing"

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

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// mockScaleTarget is a minimal ScaleTargetAccessor for GetDRAAwareGPUsPerReplica tests.
type mockScaleTarget struct {
	leader *corev1.PodTemplateSpec
	worker *corev1.PodTemplateSpec
	size   int32
	legacy int
	nsName string
}

func (m *mockScaleTarget) GetName() string                    { return "target" }
func (m *mockScaleTarget) GetNamespace() string               { return m.nsName }
func (m *mockScaleTarget) GetReplicas() *int32                { return ptr.To[int32](1) }
func (m *mockScaleTarget) GetDeletionTimestamp() *metav1.Time { return nil }
func (m *mockScaleTarget) GetStatusReplicas() int32           { return 1 }
func (m *mockScaleTarget) GetStatusReadyReplicas() int32      { return 1 }
func (m *mockScaleTarget) GetTotalGPUsPerReplica() int        { return m.legacy }
func (m *mockScaleTarget) GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec {
	if m.leader == nil {
		return m.GetWorkerPodTemplateSpec()
	}
	return m.leader
}
func (m *mockScaleTarget) GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec { return m.worker }
func (m *mockScaleTarget) GetGroupSize() int32                               { return m.size }

var _ scaletarget.ScaleTargetAccessor = (*mockScaleTarget)(nil)

func gpuTemplate(gpus string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(gpus),
					},
				},
			}},
		},
	}
}

func TestGetDRAAwareGPUsPerReplica(t *testing.T) {
	worker2 := gpuTemplate("2")

	tests := []struct {
		name   string
		target scaletarget.ScaleTargetAccessor
		want   int
	}{
		{
			name:   "nil target defaults to one",
			target: nil,
			want:   1,
		},
		{
			name:   "distinct leader and worker",
			target: &mockScaleTarget{leader: gpuTemplate("1"), worker: gpuTemplate("2"), size: 4},
			// leader(1) + (4-1)*worker(2) = 7
			want: 7,
		},
		{
			name:   "nil leader template falls back to worker and counts every pod",
			target: &mockScaleTarget{leader: nil, worker: worker2, size: 4},
			// leaderless: 4 * worker(2) = 8 (regression guard for the one-pod undercount)
			want: 8,
		},
		{
			name:   "group size one uses leader only",
			target: &mockScaleTarget{leader: gpuTemplate("3"), worker: gpuTemplate("2"), size: 1},
			want:   3,
		},
		{
			name:   "all-zero floors to one",
			target: &mockScaleTarget{leader: gpuTemplate("0"), worker: gpuTemplate("0"), size: 3},
			want:   1,
		},
	}

	c := newDRAClient(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, GetDRAAwareGPUsPerReplica(context.Background(), c, tt.target))
		})
	}
}

func TestGetDRAAwareGPUsPerReplicaFallsBackOnError(t *testing.T) {
	// A pod template referencing a claim whose Get fails (non-NotFound) makes
	// GetPodTemplateGPUs error, so we fall back to the legacy accessor value.
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "c", errors.New("forbidden"))
		},
	}).Build()

	leader := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("some-claim"),
			}},
		},
	}
	target := &mockScaleTarget{leader: leader, worker: gpuTemplate("2"), size: 1, legacy: 42}

	require.Equal(t, 42, GetDRAAwareGPUsPerReplica(context.Background(), c, target))
}

func newDRAClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, resourcev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}
