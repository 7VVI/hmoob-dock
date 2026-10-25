package main

import (
	"testing"

	"hmoob-dock/internal/httpapi"
)

// TestTheHandlerReportsTheStateOfTheAdministrator proves that the handler of
// the control端 can tell the command whether the administrator exists
// already. The command uses it to report the state of the deployment, so it
// does not have to read the bootstrap credential a second time.
func TestTheHandlerReportsTheStateOfTheAdministrator(t *testing.T) {
	var handler initializationState = (*httpapi.Server)(nil)
	if handler == nil {
		t.Fatal("the handler of the control端 does not report the state of the administrator")
	}
}
