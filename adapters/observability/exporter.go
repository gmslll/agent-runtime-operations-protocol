package observability

import "context"

// Exporter receives derived telemetry. Implementations must not be used as an
// Event ledger or Audit store; Export failure never changes protocol state.
type Exporter interface {
	Export(context.Context, Telemetry) error
}

type ExportOutcome string

const (
	ExportDisabled  ExportOutcome = "disabled"
	ExportSucceeded ExportOutcome = "succeeded"
	ExportFailed    ExportOutcome = "failed"
	ExportCancelled ExportOutcome = "cancelled"
)

// Emit sends a defensive copy to an optional exporter. The outcome is bounded
// and intentionally carries no exporter error text, which may contain secrets.
// Callers must never use it to accept, reject, commit, or roll back an AROP
// Event or Audit transaction.
func Emit(ctx context.Context, exporter Exporter, telemetry Telemetry) ExportOutcome {
	if exporter == nil {
		return ExportDisabled
	}
	if ctx == nil || ctx.Err() != nil {
		return ExportCancelled
	}
	if err := exporter.Export(ctx, cloneTelemetry(telemetry)); err != nil {
		return ExportFailed
	}
	return ExportSucceeded
}

func cloneTelemetry(source Telemetry) Telemetry {
	result := Telemetry{Metrics: make([]MetricPoint, len(source.Metrics))}
	if source.Event != nil {
		value := *source.Event
		value.Attributes = cloneAnyMap(source.Event.Attributes)
		result.Event = &value
	}
	for index, point := range source.Metrics {
		result.Metrics[index] = point
		result.Metrics[index].Attributes = make(map[string]string, len(point.Attributes))
		for key, value := range point.Attributes {
			result.Metrics[index].Attributes[key] = value
		}
	}
	return result
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
