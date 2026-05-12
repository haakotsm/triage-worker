package types

import "fmt"

// L1Commands returns suggested safe diagnostic kubectl commands for a given classification.
// These are read-only commands that an operator can copy-paste immediately.
func L1Commands(classification string, identity IncidentIdentity) []Recommendation {
	ns := identity.Namespace
	name := identity.Name
	kind := identity.Kind

	// Build label selector based on identity kind.
	// Owner-level kinds (Deployment, StatefulSet, DaemonSet) use app= label.
	// App kind uses app.kubernetes.io/name= (the label it was derived from).
	// Pod kind (normalized name, no app labels) and Namespace kind list all
	// pods in the namespace since no reliable label selector exists.
	var selector string
	switch kind {
	case "App":
		selector = fmt.Sprintf("-l app.kubernetes.io/name=%s", name)
	case "Pod", "Namespace":
		selector = "" // no reliable label — list all pods in namespace
	default:
		selector = fmt.Sprintf("-l app=%s", name)
	}

	// Events: use field selector for owner-level kinds where name matches
	// involvedObject, otherwise list namespace-wide.
	eventsCmd := fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", ns)
	if kind != "App" && kind != "Pod" && kind != "Namespace" {
		eventsCmd = fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp' --field-selector involvedObject.name=%s", ns, name)
	}

	base := []Recommendation{
		{Action: "Get pod status", Command: fmt.Sprintf("kubectl get pods -n %s %s", ns, selector), Risk: "none", Source: "l1", Expected: "Check STATUS column for CrashLoopBackOff, Error, or Pending"},
		{Action: "Describe pod", Command: fmt.Sprintf("kubectl describe pod -n %s %s", ns, selector), Risk: "none", Source: "l1", Expected: "Check Events section at bottom and Conditions table"},
		{Action: "Check recent events", Command: eventsCmd, Risk: "none", Source: "l1", Expected: "Look for Warning events with FailedScheduling, BackOff, or Unhealthy"},
	}

	switch classification {
	case "CrashLoop":
		return append(base, []Recommendation{
			{Action: "View container logs", Command: fmt.Sprintf("kubectl logs -n %s %s --tail=100 --previous", ns, selector), Risk: "none", Source: "l1", Expected: "Stack trace, panic, or fatal error near end of output"},
			{Action: "Check exit codes", Command: fmt.Sprintf("kubectl get pod -n %s %s -o jsonpath='{.items[*].status.containerStatuses[*].lastState.terminated.exitCode}'", ns, selector), Risk: "none", Source: "l1", Expected: "1=app error, 137=OOMKilled, 143=SIGTERM, 139=segfault"},
		}...)

	case "OOM":
		return append(base, []Recommendation{
			{Action: "Check memory limits", Command: fmt.Sprintf("kubectl get pod -n %s %s -o jsonpath='{.items[*].spec.containers[*].resources.limits.memory}'", ns, selector), Risk: "none", Source: "l1", Expected: "Compare limits against actual usage from next command"},
			{Action: "View memory metrics", Command: fmt.Sprintf("kubectl top pods -n %s --sort-by=memory", ns), Risk: "none", Source: "l1", Expected: "Pods using >80%% of memory limit are at risk of OOM"},
		}...)

	case "Network":
		return append(base, []Recommendation{
			{Action: "Check NetworkPolicies", Command: fmt.Sprintf("kubectl get networkpolicy -n %s", ns), Risk: "none", Source: "l1", Expected: "Missing or overly restrictive policies blocking traffic"},
			{Action: "Check service endpoints", Command: fmt.Sprintf("kubectl get endpoints -n %s", ns), Risk: "none", Source: "l1", Expected: "Empty ENDPOINTS column means no healthy backends"},
			{Action: "Check Cilium status", Command: fmt.Sprintf("kubectl exec -n kube-system -l app.kubernetes.io/name=cilium -- cilium endpoint list | grep %s", ns), Risk: "low", Source: "l1", Expected: "Endpoint state should be ready. Requires Cilium CNI — skip if using different CNI"},
		}...)

	case "ImagePull":
		return append(base, []Recommendation{
			{Action: "Check image pull secrets", Command: fmt.Sprintf("kubectl get secrets -n %s -o name | grep pull", ns), Risk: "none", Source: "l1", Expected: "Missing pull secret if image is from private registry"},
			{Action: "Verify image reference", Command: fmt.Sprintf("kubectl get %s -n %s %s -o jsonpath='{.spec.template.spec.containers[*].image}'", kindToResource(kind), ns, name), Risk: "none", Source: "l1", Expected: "Check for typos in image name or missing tag"},
		}...)

	case "ResourceExhaustion":
		return append(base, []Recommendation{
			{Action: "Check node resources", Command: "kubectl top nodes", Risk: "none", Source: "l1", Expected: "Nodes above 90%% CPU or memory indicate cluster pressure"},
			{Action: "Check pod resource usage", Command: fmt.Sprintf("kubectl top pods -n %s --sort-by=memory", ns), Risk: "none", Source: "l1", Expected: "Identify top consumers that could be scaled down"},
			{Action: "List resource limits", Command: fmt.Sprintf("kubectl get pods -n %s -o custom-columns=NAME:.metadata.name,CPU_REQ:.spec.containers[*].resources.requests.cpu,MEM_REQ:.spec.containers[*].resources.requests.memory", ns), Risk: "none", Source: "l1", Expected: "Pods without requests may be evicted first under pressure"},
		}...)

	case "Config":
		return append(base, []Recommendation{
			{Action: "Check ConfigMaps", Command: fmt.Sprintf("kubectl get configmap -n %s", ns), Risk: "none", Source: "l1", Expected: "Missing or recently modified ConfigMaps"},
			{Action: "Check Secrets", Command: fmt.Sprintf("kubectl get secrets -n %s", ns), Risk: "none", Source: "l1", Expected: "Missing secrets referenced by pod volumes or env"},
			{Action: "View env vars", Command: fmt.Sprintf("kubectl set env %s/%s --list -n %s", kindToResource(kind), name, ns), Risk: "none", Source: "l1", Expected: "Missing or incorrect environment variable values"},
		}...)

	case "Scheduling":
		return append(base, []Recommendation{
			{Action: "Check pending pods", Command: fmt.Sprintf("kubectl get pods -n %s --field-selector status.phase=Pending", ns), Risk: "none", Source: "l1", Expected: "Pending pods indicate scheduling failures"},
			{Action: "Check node taints", Command: "kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints", Risk: "none", Source: "l1", Expected: "Taints without matching tolerations prevent scheduling"},
			{Action: "Check resource quotas", Command: fmt.Sprintf("kubectl describe resourcequota -n %s", ns), Risk: "none", Source: "l1", Expected: "Used vs hard limits — quota exceeded blocks new pods"},
		}...)
	}

	return base
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
