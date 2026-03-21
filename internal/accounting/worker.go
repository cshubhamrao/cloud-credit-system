package accounting

import (
	"fmt"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// NewClient creates a Temporal client.
func NewClient(host, namespace string) (client.Client, error) {
	c, err := client.Dial(client.Options{
		HostPort:  host,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("temporal.Dial: %w", err)
	}
	return c, nil
}

// StartWorker registers all workflows and activities and starts the Temporal worker.
func StartWorker(c client.Client, tbActs *TBActivities, pgActs *PGActivities) worker.Worker {
	w := worker.New(c, TaskQueueAccounting, worker.Options{})

	// Workflows
	w.RegisterWorkflow(TenantAccountingWorkflow)
	w.RegisterWorkflow(TenantProvisioningWorkflow)
	w.RegisterWorkflow(RegisterClusterWorkflow)

	// Activities — register by struct so all methods are registered automatically.
	// Names are derived from method names via reflection, matching the typed
	// references used in workflow.ExecuteActivity calls.
	w.RegisterActivity(tbActs)
	w.RegisterActivity(pgActs)

	return w
}
