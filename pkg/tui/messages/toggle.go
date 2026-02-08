package messages

// UI toggle messages control various UI state toggles.
type (
	// ToggleYoloMsg toggles YOLO mode (auto-approve tools).
	ToggleYoloMsg struct{}

	// ToggleThinkingMsg toggles extended thinking mode.
	ToggleThinkingMsg struct{}

	// ToggleHideToolResultsMsg toggles hiding of tool results.
	ToggleHideToolResultsMsg struct{}

	// ToggleSidebarMsg toggles sidebar visibility.
	// The background model also handles this to persist the collapsed state.
	ToggleSidebarMsg struct{}

	// ShowCostDialogMsg shows the cost/usage dialog.
	ShowCostDialogMsg struct{}

	// ShowPermissionsDialogMsg shows the permissions dialog.
	ShowPermissionsDialogMsg struct{}
)
