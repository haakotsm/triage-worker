package types

import (
	"fmt"
	"strings"
)

// VerificationCommands returns read-only diagnostic commands that let a
// responder confirm the agent's diagnosis. Rather than a fixed per-classification
// list, the set is seeded by what the agent actually found — its classification
// and the enrichment facts it observed (exit codes, memory %, restart rate) —
// and targets the real pods named in the firing alerts (falling back to a label
// selector). Each command states which finding it verifies, so the list reads
// as "run these to confirm the diagnosis." All commands are read-only.
//
// Source is set to "l1" so NormalizeRecommendations and the web handler keep
// partitioning these ahead of the agent's remediation recommendations.
func VerificationCommands(report TriageReport, identity IncidentIdentity, enrichment EnrichmentResult, alerts []Alert) []Recommendation {
	ns := identity.Namespace
	podArg, logsArg := podTargets(alerts, identity)
	k8s := enrichment.Kubernetes
	prom := enrichment.Prometheus
	loki := enrichment.Loki

	// De-dup by Action so a check selected by both classification and an
	// enrichment signal appears once; insertion order is preserved.
	seen := make(map[string]bool)
	var cmds []Recommendation
	add := func(r Recommendation) {
		if r.Action == "" || seen[r.Action] {
			return
		}
		seen[r.Action] = true
		r.Source = "l1"
		if r.Risk == "" {
			r.Risk = "none"
		}
		cmds = append(cmds, r)
	}

	// --- Base: confirm the workload is actually unhealthy ---
	statusExpected := "Look for non-Running pods (CrashLoopBackOff, Error, Pending)."
	if k8s.Available && k8s.PodPhase != "" {
		statusExpected = fmt.Sprintf("Agent saw pod phase %q — confirm it here.", k8s.PodPhase)
	}
	add(Recommendation{Action: "Confirm pod status", Command: fmt.Sprintf("kubectl get pods -n %s %s -o wide", ns, podArg), Expected: statusExpected})
	add(Recommendation{Action: "Inspect pod & conditions", Command: fmt.Sprintf("kubectl describe pod -n %s %s", ns, podArg), Expected: "Check the Conditions table and Events against the diagnosis."})

	// --- Logs: when the diagnosis rests on crashes or log errors ---
	if logsArg != "" && (report.Classification == "CrashLoop" || report.Classification == "Config" || len(loki.ErrorLines) > 0) {
		prevFlag := ""
		if report.Classification == "CrashLoop" {
			prevFlag = " --previous"
		}
		exp := "Look for the error described in the agent's root cause."
		if loki.Available && loki.LogCount > 0 {
			exp = fmt.Sprintf("Agent cited %d error log line(s) — confirm the same errors appear.", loki.LogCount)
		}
		add(Recommendation{Action: "Read application logs", Command: fmt.Sprintf("kubectl logs -n %s %s --tail=100%s", ns, logsArg, prevFlag), Expected: exp})
	}

	// --- Exit codes: whenever a termination was observed or implied ---
	if len(k8s.ExitCodes) > 0 || report.Classification == "CrashLoop" || report.Classification == "OOM" {
		exp := "Decode: 137=OOMKilled, 1=app error, 143=SIGTERM, 139=segfault."
		if len(k8s.ExitCodes) > 0 {
			exp = fmt.Sprintf("Agent observed exit code(s) %v — %s", k8s.ExitCodes, exp)
		}
		add(Recommendation{Action: "Confirm container exit code", Command: fmt.Sprintf("kubectl get pod -n %s %s -o jsonpath='{.items[*].status.containerStatuses[*].lastState.terminated.exitCode}'", ns, podArg), Expected: exp})
	}

	// --- Memory: when OOM, or metrics showed pressure ---
	if report.Classification == "OOM" || (prom.Available && prom.MemoryPct >= 80) {
		usageExp := "Pods near 100% of their memory limit are at OOM risk."
		if prom.Available && prom.MemoryPct > 0 {
			usageExp = fmt.Sprintf("Agent measured memory at %.0f%% of limit — confirm it's still elevated.", prom.MemoryPct)
		}
		add(Recommendation{Action: "Check memory limits", Command: fmt.Sprintf("kubectl get pod -n %s %s -o jsonpath='{.items[*].spec.containers[*].resources.limits.memory}'", ns, podArg), Expected: "Compare the limit against actual usage from the next command."})
		add(Recommendation{Action: "Check live memory usage", Command: fmt.Sprintf("kubectl top pods -n %s --sort-by=memory", ns), Expected: usageExp})
	}

	// --- Restart rate: when metrics observed restarts ---
	if prom.Available && prom.RestartRate > 0 {
		add(Recommendation{Action: "Confirm restart count", Command: fmt.Sprintf("kubectl get pods -n %s %s -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}'", ns, podArg), Expected: fmt.Sprintf("Agent measured a 5m restart rate of %.1f — restart counts should be climbing.", prom.RestartRate)})
	}

	// --- Classification-specific corroboration ---
	switch report.Classification {
	case "Network":
		add(Recommendation{Action: "Check NetworkPolicies", Command: fmt.Sprintf("kubectl get networkpolicy -n %s", ns), Expected: "A missing or over-restrictive policy would block the traffic the agent flagged."})
		add(Recommendation{Action: "Check service endpoints", Command: fmt.Sprintf("kubectl get endpoints -n %s", ns), Expected: "Empty ENDPOINTS means no healthy backends — matches a connectivity cause."})
	case "ImagePull":
		add(Recommendation{Action: "Verify image reference", Command: fmt.Sprintf("kubectl get %s -n %s %s -o jsonpath='{.spec.template.spec.containers[*].image}'", kindToResource(identity.Kind), ns, identity.Name), Expected: "Check the image/tag the agent says cannot be pulled for a typo or bad tag."})
		add(Recommendation{Action: "Check image pull secrets", Command: fmt.Sprintf("kubectl get secrets -n %s -o name | grep pull", ns), Expected: "A missing pull secret for a private registry confirms an ImagePull cause."})
	case "ResourceExhaustion":
		add(Recommendation{Action: "Check node resources", Command: "kubectl top nodes", Expected: "Nodes above ~90% CPU/memory corroborate cluster pressure."})
	case "Config":
		add(Recommendation{Action: "Check ConfigMaps & Secrets", Command: fmt.Sprintf("kubectl get configmap,secret -n %s", ns), Expected: "A missing or renamed ConfigMap/Secret matches a config cause."})
	case "Scheduling":
		add(Recommendation{Action: "Check pending pods", Command: fmt.Sprintf("kubectl get pods -n %s --field-selector status.phase=Pending", ns), Expected: "Pending pods indicate the scheduling failure the agent diagnosed."})
		add(Recommendation{Action: "Check node taints", Command: "kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints", Expected: "Taints without matching tolerations prevent scheduling."})
	}

	// --- Always: recent events as a catch-all corroboration ---
	add(Recommendation{Action: "Check recent events", Command: fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", ns), Expected: "Warning events (BackOff, FailedScheduling, Unhealthy) should align with the diagnosis."})

	return cmds
}

// podTargets returns the kubectl target args for the affected pods. When the
// firing alerts name real pods it targets them directly (so logs land on the
// right pod); otherwise it falls back to a label selector from the workload
// identity. podArg is for commands that accept multiple pods (get/describe/
// jsonpath); logsArg is for `kubectl logs`, which takes a single pod or a
// selector — empty when neither is available.
func podTargets(alerts []Alert, id IncidentIdentity) (podArg, logsArg string) {
	var pods []string
	seen := make(map[string]bool)
	for _, a := range alerts {
		// These commands are meant to be copy-pasted into a shell, so sanitize
		// the (untrusted) alert pod label to a valid K8s name — a hostile label
		// like "x; rm -rf /" must not become a runnable command.
		p := SanitizeK8sName(a.Labels["pod"])
		if a.Labels["pod"] != "" && p != "" && p != "unknown" && !seen[p] {
			seen[p] = true
			pods = append(pods, p)
		}
	}
	if len(pods) > 0 {
		return strings.Join(pods, " "), pods[0]
	}
	sel := labelSelector(id)
	return sel, sel
}

// labelSelector derives a best-effort pod selector from the workload identity
// when no concrete pod names are available.
func labelSelector(id IncidentIdentity) string {
	switch id.Kind {
	case "App":
		return fmt.Sprintf("-l app.kubernetes.io/name=%s", id.Name)
	case "Pod", "Namespace":
		return "" // no reliable label — operate namespace-wide
	default:
		return fmt.Sprintf("-l app=%s", id.Name)
	}
}

func kindToResource(kind string) string {
	switch kind {
	case "Deployment":
		return "deployment"
	case "StatefulSet":
		return "statefulset"
	case "DaemonSet":
		return "daemonset"
	case "Job":
		return "job"
	case "CronJob":
		return "cronjob"
	default:
		return "pod"
	}
}
