package accounting

// tbActivities and pgActivities are nil-value stubs used for type-safe activity
// name resolution in workflow.ExecuteActivity calls. The SDK resolves method names
// via reflection — the nil receiver is never invoked.
var (
	tbActivities *TBActivities
	pgActivities *PGActivities
)
