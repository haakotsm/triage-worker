package types

import "fmt"

// L1Commands returns suggested safe diagnostic kubectl commands for a given classification.
// These are read-only commands that an operator can copy-paste immediately.
func L1Commands(classification string, identity IncidentIdentity) []Recommendation {
	ns := identity.Namespace
	name := identity.Name
	kind := identity.Kind

	// Build label selector based on identity kind.
	// Owner-level kinds use app= label; App/Pod use pod prefix match.
	selector := fmt.Sprintf("-l app=%s", name)
	if kind == "App" || kind == "Pod" || kind == "Namespace" {
		selector = fmt.Sprintf("-l app.kubernetes.io/name=%s", name)
	}

	base := []Recommendation{
		{Action: "Get pod status", Command: fmt.Sprintf("kubectl get pods -n %s %s", ns, selector), Risk: "none"},
		{Action: "Describe pod", Command: fmt.Sprintf("kubectl describe pod -n %s %s", ns, selector), Risk: "none"},
		{Action: "Check recent events", Command: fmt.Sprintf("kubectl get events -n %s --sort-by='.lastTimestamp'", ns), Risk: "none"},
	}

	switch classification {
	case "CrashLoop":
		return append(base, []Recommendation{
			{Action: "View container logs", Command: fmt.Sprintf("kubectl logs -n %s -l app=%s --tail=100 --previous", ns, name), Risk: "none"},
			{Action: "Check exit codes", Command: fmt.Sprintf("kubectl get pod -n %s -l app=%s -o jsonpath='{.items[*].status.containerStatuses[*].lastState.terminated.exitCode}'", ns, name), Risk: "none"},
		}...)

	case "OOM":
		return append(base, []Recommendation{
			{Action: "Check memory limits", Command: fmt.Sprintf("kubectl get pod -n %s -l app=%s -o jsonpath='{.items[*].spec.containers[*].resources.limits.memory}'", ns, name), Risk: "none"},
			{Action: "View memory metrics", Command: fmt.Sprintf("kubectl top pods -n %s -l app=%s", ns, name), Risk: "none"},
		}...)

	case "Network":
		return append(base, []Recommendation{
			{Action: "Check NetworkPolicies", Command: fmt.Sprintf("kubectl get networkpolicy -n %s", ns), Risk: "none"},
			{Action: "Check service endpoints", Command: fmt.Sprintf("kubectl get endpoints -n %s", ns), Risk: "none"},
			{Action: "Check Cilium status", Command: fmt.Sprintf("kubectl exec -n kube-system -l app.kubernetes.io/name=cilium -- cilium endpoint list | grep %s", ns), Risk: "none"},
		}...)

	case "ImagePull":
		return append(base, []Recommendation{
			{Action: "Check image pull secrets", Command: fmt.Sprintf("kubectl get secrets -n %s -o name | grep pull", ns), Risk: "none"},
			{Action: "Verify image reference", Command: fmt.Sprintf("kubectl get %s -n %s %s -o jsonpath='{.spec.template.spec.containers[*].image}'", kindToResource(kind), ns, name), Risk: "none"},
		}...)

	case "ResourceExhaustion":
		return append(base, []Recommendation{
			{Action: "Check node resources", Command: "kubectl top nodes", Risk: "none"},
			{Action: "Check pod resource usage", Command: fmt.Sprintf("kubectl top pods -n %s --sort-by=memory", ns), Risk: "none"},
			{Action: "List resource limits", Command: fmt.Sprintf("kubectl get pods -n %s -o custom-columns=NAME:.metadata.name,CPU_REQ:.spec.containers[*].resources.requests.cpu,MEM_REQ:.spec.containers[*].resources.requests.memory", ns), Risk: "none"},
		}...)

	case "Config":
		return append(base, []Recommendation{
			{Action: "Check ConfigMaps", Command: fmt.Sprintf("kubectl get configmap -n %s", ns), Risk: "none"},
			{Action: "Check Secrets", Command: fmt.Sprintf("kubectl get secrets -n %s", ns), Risk: "none"},
			{Action: "View env vars", Command: fmt.Sprintf("kubectl set env %s/%s --list -n %s", kindToResource(kind), name, ns), Risk: "none"},
		}...)

	case "Scheduling":
		return append(base, []Recommendation{
			{Action: "Check pending pods", Command: fmt.Sprintf("kubectl get pods -n %s --field-selector status.phase=Pending", ns), Risk: "none"},
			{Action: "Check node taints", Command: "kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints", Risk: "none"},
			{Action: "Check resource quotas", Command: fmt.Sprintf("kubectl describe resourcequota -n %s", ns), Risk: "none"},
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
