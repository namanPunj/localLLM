package server

import (
	"context"
	"testing"
)

func TestExecuteAddTask_Live(t *testing.T) {
	// Make sure credentials.json is accessible from where you run the test
	client, err := getTasksClient(context.Background())
	if err != nil {
		t.Fatalf("Failed to init tasks client: %v", err)
	}

	executor := &ToolExecutor{
		// Update this if you used the TaskAdder interface from the previous step
		TasksService: client, 
	}

	args := map[string]any{
		"title":    "Test task from Go test!",
		"due_date": "2026-05-16T10:00:00Z", 
	}

	err = executor.Execute(context.Background(), "add_task", args)
	if err != nil {
		t.Errorf("Execute failed: %v", err)
	}
}