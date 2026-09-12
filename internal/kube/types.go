// Package kube is a minimal, stdlib-only REST client for the Kubernetes
// API. It implements only the verbs and types this controller needs
// (PLAN-M1.md 3.2): no watch, polling-based usage, hand-rolled types.
package kube

import (
	"errors"
	"time"
)

// Time is a JSON time with optional fractional seconds (Kubernetes uses
// RFC3339 with optional microseconds).
type Time time.Time

// UnmarshalJSON accepts RFC3339 with or without fractional seconds.
func (t *Time) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "null" || s == "" {
		*t = Time{}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = Time(v)
			return nil
		}
	}
	return errors.New("kube: cannot parse time " + string(b))
}

func (t Time) MarshalJSON() ([]byte, error) {
	if (Time{}) == t {
		return []byte("null"), nil
	}
	return []byte(`"` + time.Time(t).UTC().Format(time.RFC3339) + `"`), nil
}

// MicroTime is a metav1.MicroTime: RFC3339 with six fractional digits
// (layout 2006-01-02T15:04:05.000000Z07:00). Lease spec fields use
// MicroTime, not metav1.Time, so plain RFC3339 seconds are rejected by
// the API server.
type MicroTime time.Time

const microTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func (t *MicroTime) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "null" || s == "" {
		*t = MicroTime{}
		return nil
	}
	for _, layout := range []string{microTimeLayout, time.RFC3339Nano, time.RFC3339} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = MicroTime(v)
			return nil
		}
	}
	return errors.New("kube: cannot parse microtime " + string(b))
}

func (t MicroTime) MarshalJSON() ([]byte, error) {
	if (MicroTime{}) == t {
		return []byte("null"), nil
	}
	return []byte(`"` + time.Time(t).UTC().Format(microTimeLayout) + `"`), nil
}

// Time returns the underlying time.Time (zero MicroTime -> zero time).
func (t MicroTime) Time() time.Time { return time.Time(t) }

// ObjectMeta carries only the metadata fields the controller uses.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	GenerateName      string            `json:"generateName,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	DeletionTimestamp *Time             `json:"deletionTimestamp,omitempty"`
}

// Terminating reports whether a deletion has been requested.
func (m ObjectMeta) Terminating() bool { return m.DeletionTimestamp != nil }

// OwnerReference is a subset of metav1.OwnerReference.
type OwnerReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Node is a minimal v1 Node.
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Conditions []NodeCondition `json:"conditions"`
		NodeInfo   NodeInfo        `json:"nodeInfo"`
	} `json:"status"`
}

// NodeInfo is the subset of v1.NodeSystemInfo the controller uses
// (PLAN-M2 3.6: the running kernel version and the machine architecture).
type NodeInfo struct {
	KernelVersion string `json:"kernelVersion,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
}

// NodeCondition is a subset of v1.NodeCondition.
type NodeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// Ready reports the node's Ready condition (false when absent).
func (n *Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// IsControlPlane reports the node-role.kubernetes.io/control-plane label.
func (n *Node) IsControlPlane() bool {
	_, ok := n.Metadata.Labels["node-role.kubernetes.io/control-plane"]
	return ok
}

// Pod is a minimal v1 Pod.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		NodeName                      string `json:"nodeName"`
		TerminationGracePeriodSeconds int64  `json:"terminationGracePeriodSeconds"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// Running reports phase Running.
func (p *Pod) Running() bool { return p.Status.Phase == "Running" }

// PodDisruptionBudget is a minimal policy/v1 PDB.
type PodDisruptionBudget struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Selector *LabelSelector `json:"selector"`
	} `json:"spec"`
	Status struct {
		DisruptionsAllowed int32 `json:"disruptionsAllowed"`
	} `json:"status"`
}

// LabelSelector mirrors metav1.LabelSelector (used by PDBs and the
// DaemonSet pod lookup).
type LabelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels,omitempty"`
	MatchExpressions []SelectorReq     `json:"matchExpressions,omitempty"`
}

// SelectorReq mirrors metav1.LabelSelectorRequirement.
type SelectorReq struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// Matches reports whether the given pod labels satisfy the selector.
// A nil selector matches everything.
func (s *LabelSelector) Matches(labels map[string]string) bool {
	if s == nil {
		return true
	}
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, r := range s.MatchExpressions {
		if !matchRequirement(r, labels) {
			return false
		}
	}
	return true
}

func matchRequirement(r SelectorReq, labels map[string]string) bool {
	v, present := labels[r.Key]
	switch r.Operator {
	case "In":
		if !present {
			return false
		}
		for _, want := range r.Values {
			if v == want {
				return true
			}
		}
		return false
	case "NotIn":
		if !present {
			return true
		}
		for _, want := range r.Values {
			if v == want {
				return false
			}
		}
		return true
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	default: // "Equals", "NotEquals"
		if r.Operator == "NotEquals" {
			return v != r.Values[0]
		}
		return present && len(r.Values) > 0 && v == r.Values[0]
	}
}

// Lease is a minimal coordination.k8s.io/v1 Lease.
type Lease struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		HolderIdentity string     `json:"holderIdentity,omitempty"`
		RenewTime      *MicroTime `json:"renewTime,omitempty"`
	} `json:"spec"`
}

// ConfigMap is a minimal v1 ConfigMap (flat string data, PLAN-M2 3.2).
type ConfigMap struct {
	Metadata ObjectMeta        `json:"metadata"`
	Data     map[string]string `json:"data,omitempty"`
}

// Event is a minimal v1 Event (namespaced; created regarding a Node).
type Event struct {
	Metadata       ObjectMeta `json:"metadata"`
	InvolvedObject struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		UID        string `json:"uid,omitempty"`
	} `json:"involvedObject"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Source  struct {
		Component string `json:"component"`
	} `json:"source"`
	Type           string `json:"type,omitempty"`
	FirstTimestamp Time   `json:"firstTimestamp"`
	LastTimestamp  Time   `json:"lastTimestamp"`
	Count          int32  `json:"count"`
}

// Eviction is a minimal policy/v1 Eviction body.
type Eviction struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
}

// NodeList / PodList / PDBList are list envelopes.
type NodeList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []Node `json:"items"`
}

type PodList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []Pod `json:"items"`
}

type PDBList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []PodDisruptionBudget `json:"items"`
}
