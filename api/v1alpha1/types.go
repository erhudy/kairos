package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Run",type=string,JSONPath=`.status.lastRunTime`,format=date-time
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RestartChain is a cluster-scoped resource that orchestrates sequential restarts
// of multiple Kubernetes workloads across namespaces.
type RestartChain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestartChainSpec   `json:"spec,omitempty"`
	Status RestartChainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestartChainList contains a list of RestartChain.
type RestartChainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RestartChain `json:"items"`
}

// RestartChainSpec defines the desired state of a RestartChain.
type RestartChainSpec struct {
	// CronPattern is a standard 5-field or 6-field (with seconds) cron expression.
	CronPattern string `json:"cronPattern"`

	// Paused suspends future executions when set to true.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// Steps defines the ordered sequence of resources to restart.
	// +kubebuilder:validation:MinItems=1
	Steps []RestartChainStep `json:"steps"`
}

// RestartChainStep defines a single step in the restart chain.
type RestartChainStep struct {
	// ResourceRef identifies the workload to restart.
	ResourceRef ResourceRef `json:"resourceRef"`

	// HealthCheck configures how to determine if the restart was successful.
	// +optional
	HealthCheck HealthCheck `json:"healthCheck,omitempty"`
}

// ResourceRef identifies a Kubernetes workload resource.
type ResourceRef struct {
	// Kind is the resource kind: Deployment, DaemonSet, or StatefulSet.
	// +kubebuilder:validation:Enum=Deployment;DaemonSet;StatefulSet
	Kind string `json:"kind"`

	// Name is the resource name.
	Name string `json:"name"`

	// Namespace is the resource namespace.
	Namespace string `json:"namespace"`
}

// HealthCheckStrategy defines how to check if a restart was successful.
// +kubebuilder:validation:Enum=RolloutComplete;FixedTimeout;RolloutWithTimeout
type HealthCheckStrategy string

const (
	// HealthCheckRolloutComplete waits until the rollout is fully complete.
	HealthCheckRolloutComplete HealthCheckStrategy = "RolloutComplete"

	// HealthCheckFixedTimeout waits a fixed duration then proceeds.
	HealthCheckFixedTimeout HealthCheckStrategy = "FixedTimeout"

	// HealthCheckRolloutWithTimeout polls for rollout completion but proceeds after timeout.
	HealthCheckRolloutWithTimeout HealthCheckStrategy = "RolloutWithTimeout"
)

// HealthCheck configures health checking for a restart step.
type HealthCheck struct {
	// Strategy determines how health is checked.
	// +optional
	// +kubebuilder:default=RolloutComplete
	Strategy HealthCheckStrategy `json:"strategy,omitempty"`

	// TimeoutSeconds is the maximum time to wait for health check completion.
	// +optional
	// +kubebuilder:default=600
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// PollIntervalSeconds is how often to check health status.
	// +optional
	// +kubebuilder:default=5
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`
}

// ChainPhase represents the current phase of a RestartChain execution.
type ChainPhase string

const (
	ChainPhaseIdle      ChainPhase = "Idle"
	ChainPhaseRunning   ChainPhase = "Running"
	ChainPhaseCompleted ChainPhase = "Completed"
	ChainPhaseFailed    ChainPhase = "Failed"
)

// StepPhase represents the current phase of a single step.
type StepPhase string

const (
	StepPhasePending           StepPhase = "Pending"
	StepPhaseRestarting        StepPhase = "Restarting"
	StepPhaseWaitingForHealthy StepPhase = "WaitingForHealthy"
	StepPhaseCompleted         StepPhase = "Completed"
	StepPhaseFailed            StepPhase = "Failed"
)

// RestartChainStatus defines the observed state of a RestartChain.
type RestartChainStatus struct {
	// Phase is the current phase of the chain execution.
	// +optional
	Phase ChainPhase `json:"phase,omitempty"`

	// CurrentStep is the index of the currently executing step (-1 if idle).
	// +optional
	CurrentStep int32 `json:"currentStep"`

	// LastRunTime is when the chain was last triggered.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// LastCompletionTime is when the chain last completed (successfully or not).
	// +optional
	LastCompletionTime *metav1.Time `json:"lastCompletionTime,omitempty"`

	// StepStatuses tracks the status of each step.
	// +optional
	StepStatuses []StepStatus `json:"stepStatuses,omitempty"`

	// Conditions represent the latest available observations of the chain's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StepStatus tracks the status of a single step in the chain.
type StepStatus struct {
	// ResourceRef identifies which resource this status is for.
	ResourceRef ResourceRef `json:"resourceRef"`

	// Phase is the current phase of this step.
	Phase StepPhase `json:"phase"`

	// StartedAt is when this step started executing.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when this step finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Message provides human-readable details about the step status.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the resource's Generation at the time of restart,
	// used to detect when the rollout has completed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
