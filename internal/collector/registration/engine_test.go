package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

var _ = Describe("EngineQuery", func() {
	It("returns the bare logical name for vLLM (backward compatible)", func() {
		Expect(EngineQuery(inferenceengine.EngineVLLM, QueryKvCacheUsage)).To(Equal(QueryKvCacheUsage))
	})

	It("prefixes engine-specific queries for SGLang", func() {
		Expect(EngineQuery(inferenceengine.EngineSGLang, QueryKvCacheUsage)).To(Equal("sglang/" + QueryKvCacheUsage))
	})

	It("returns the bare name for engine-agnostic queries regardless of engine", func() {
		// Scheduler queries are not engine-specific (sourced from EPP).
		Expect(IsEngineSpecific(QuerySchedulerDispatchRate)).To(BeFalse())
		Expect(EngineQuery(inferenceengine.EngineSGLang, QuerySchedulerDispatchRate)).To(Equal(QuerySchedulerDispatchRate))
	})
})

var _ = Describe("SGLang query registration", func() {
	var registry *source.SourceRegistry

	BeforeEach(func() {
		ctx := context.Background()
		registry = source.NewSourceRegistry()
		metricsSource := prometheus.NewPrometheusSource(ctx, &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
		Expect(registry.Register("prometheus", metricsSource)).NotTo(HaveOccurred())
		RegisterSaturationQueries(registry)
		RegisterQueueingModelQueries(registry)
		RegisterThroughputAnalyzerQueries(registry)
		RegisterScaleToZeroQueries(registry)
	})

	get := func(engine inferenceengine.Engine, logical string) *source.QueryTemplate {
		return registry.Get("prometheus").QueryList().Get(EngineQuery(engine, logical))
	}

	It("registers both vLLM and SGLang variants of engine-specific queries", func() {
		for _, logical := range EngineSpecificQueries {
			Expect(get(inferenceengine.EngineVLLM, logical)).NotTo(BeNil(), "missing vLLM "+logical)
			Expect(get(inferenceengine.EngineSGLang, logical)).NotTo(BeNil(), "missing SGLang "+logical)
		}
	})

	It("uses sglang:* metric names in SGLang templates", func() {
		Expect(get(inferenceengine.EngineSGLang, QueryKvCacheUsage).Template).To(ContainSubstring("sglang:token_usage"))
		Expect(get(inferenceengine.EngineSGLang, QueryQueueLength).Template).To(ContainSubstring("sglang:num_queue_reqs"))
		Expect(get(inferenceengine.EngineSGLang, QueryCacheConfigInfo).Template).To(ContainSubstring("sglang:max_total_num_tokens"))
		Expect(get(inferenceengine.EngineSGLang, QueryAvgTTFT).Template).To(ContainSubstring("sglang:time_to_first_token_seconds_sum"))
		Expect(get(inferenceengine.EngineSGLang, QueryAvgITL).Template).To(ContainSubstring("sglang:inter_token_latency_seconds_sum"))
		Expect(get(inferenceengine.EngineSGLang, QueryModelRequestCount).Template).To(ContainSubstring("sglang:num_requests_total"))
	})

	It("keeps SGLang templates free of vllm:* metric names", func() {
		for _, logical := range EngineSpecificQueries {
			Expect(get(inferenceengine.EngineSGLang, logical).Template).NotTo(ContainSubstring("vllm:"), "SGLang "+logical+" leaks a vllm: metric")
		}
	})
})
