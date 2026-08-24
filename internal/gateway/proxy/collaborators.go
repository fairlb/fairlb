package proxy

// Admission owns the part of the request state machine that is identical for
// streaming and buffered delivery: authentication, policy gates, model
// resolution, the immutable pricing snapshot and hold estimation.
type Admission struct {
	pipeline *Pipeline
	pricing  *PricingSnapshot
}

// PricingSnapshot resolves every price input from one MVCC snapshot. Keeping
// it separate makes it impossible for execution or settlement to silently
// re-read a newer price version halfway through a request.
type PricingSnapshot struct {
	pipeline *Pipeline
}

// Executor owns upstream credential selection, retries and failover. Delivery
// remains different for streaming responses, but both modes consume the same
// admitted request and the same rotation state machine.
type Executor struct {
	pipeline *Pipeline
}

// SettlementRecorder is the only collaborator allowed to release holds or
// persist successful, rejected, pricing-missing and unsettled outcomes.
type SettlementRecorder struct {
	pipeline *Pipeline
}
