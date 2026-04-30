package workflow

import (
	"go.temporal.io/sdk/temporal"
)

// Search attribute key definitions for Temporal visibility.
// These must be pre-registered in the Temporal namespace.
var (
	NamespaceKey      = temporal.NewSearchAttributeKeyString("TriageNamespace")
	WorkloadKey       = temporal.NewSearchAttributeKeyString("TriageWorkload")
	ClassificationKey = temporal.NewSearchAttributeKeyString("TriageClassification")
	SeverityKey       = temporal.NewSearchAttributeKeyString("TriageSeverity")
)
