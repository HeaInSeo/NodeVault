package kubeutil

// DangerousNamespaces are cluster-critical namespaces that must not be a
// default measurement/cleanup target.
var DangerousNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// IsDangerousNamespace reports whether ns is a cluster-critical namespace.
// Callers that issue kubectl operations against a caller-supplied namespace
// should reject ns when this returns true, unless the caller has explicitly
// opted in via a Dangerously*-named override.
func IsDangerousNamespace(ns string) bool {
	return DangerousNamespaces[ns]
}
