package v1

const (
	// AuthenticationReady reports whether downstream authentication resources are ready.
	AuthenticationReady RayClusterConditionType = "AuthenticationReady"
	// TargetClusterChanged indicates an in-progress upgrade was rolled back after its goal state changed.
	TargetClusterChanged RayServiceConditionReason = "TargetClusterChanged"
)
