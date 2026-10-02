package main

import (
	"fmt"
	"net/http"
	"sync"
)

func runShiftHandover(client *apiClient) error {
	if err := verifySuccessfulHandover(client); err != nil {
		return err
	}
	if err := verifyTerminalCommandsCannotTransfer(client, "completed"); err != nil {
		return err
	}
	if err := verifyTerminalCommandsCannotTransfer(client, "aborted"); err != nil {
		return err
	}
	if err := verifyNonApprovedCommandsCannotTransfer(client); err != nil {
		return err
	}
	if err := verifyConcurrentHandover(client); err != nil {
		return err
	}
	if err := verifyHandoverExecuteRace(client); err != nil {
		return err
	}

	fmt.Println("shift-handover: permission transferred, old credential revoked, races single-winner")
	return nil
}

func approveFixtureCommand(
	client *apiClient,
	gateID string,
	targetAperture float64,
) (commandID, token string, err error) {
	if _, err = fixtureGate(client, gateID, "闸门-"+gateID, 4, 0.25); err != nil {
		return "", "", err
	}
	command, err := fixtureCommand(client, gateID, targetAperture, "交接班前已核准命令", "dispatcher-zhao")
	if err != nil {
		return "", "", err
	}
	commandID, err = stringField(command, "id")
	if err != nil {
		return "", "", err
	}
	approved, err := fixtureClearReview(client, commandID, "safety-he")
	if err != nil {
		return "", "", err
	}
	token, err = stringField(approved, "execution_token")
	if err != nil {
		return "", "", err
	}
	return commandID, token, nil
}

func handoverCommand(client *apiClient, commandID, supervisor, operator, note string) (apiResult, error) {
	return client.post("/commands/"+commandID+"/handover", map[string]any{
		"shift_supervisor":  supervisor,
		"incoming_operator": operator,
		"note":              note,
	})
}

func executeCommand(client *apiClient, commandID, operator, token string, simulateFault bool) (apiResult, error) {
	return client.post("/commands/"+commandID+"/execute", map[string]any{
		"operator":       operator,
		"token":          token,
		"simulate_fault": simulateFault,
	})
}

func verifySuccessfulHandover(client *apiClient) error {
	commandID, oldToken, err := approveFixtureCommand(client, "gate-handover-a", 2.0)
	if err != nil {
		return err
	}

	handover, err := handoverCommand(client, commandID, "supervisor-gao", "operator-bai", "中控交接班，闸门尚未动作")
	if err != nil {
		return err
	}
	if err := requireStatus(handover, http.StatusOK, "hand over approved command"); err != nil {
		return err
	}

	handedCommand, err := objectField(handover.Body, "command")
	if err != nil {
		return err
	}
	status, err := stringField(handedCommand, "status")
	if err != nil {
		return err
	}
	if status != "approved" {
		return fmt.Errorf("handover status: expected approved, got %q", status)
	}
	newToken, err := stringField(handedCommand, "execution_token")
	if err != nil {
		return err
	}
	if newToken == "" || newToken == oldToken {
		return fmt.Errorf("handover must issue a fresh execution token (old=%q new=%q)", oldToken, newToken)
	}
	authorized, err := stringField(handedCommand, "authorized_operator")
	if err != nil {
		return err
	}
	if authorized != "operator-bai" {
		return fmt.Errorf("authorized operator: expected operator-bai, got %q", authorized)
	}
	record, err := objectField(handedCommand, "handover")
	if err != nil {
		return err
	}
	if supervisor, err := stringField(record, "shift_supervisor"); err != nil || supervisor != "supervisor-gao" {
		return fmt.Errorf("handover record supervisor mismatch: %v", err)
	}
	if incoming, err := stringField(record, "incoming_operator"); err != nil || incoming != "operator-bai" {
		return fmt.Errorf("handover record operator mismatch: %v", err)
	}

	handedGate, err := objectField(handover.Body, "gate")
	if err != nil {
		return err
	}
	gateStatus, err := stringField(handedGate, "status")
	if err != nil {
		return err
	}
	activeCommand, err := stringField(handedGate, "active_command_id")
	if err != nil {
		return err
	}
	if gateStatus != "reserved" || activeCommand != commandID {
		return fmt.Errorf("gate occupancy after handover: status=%q active=%q", gateStatus, activeCommand)
	}

	stale, err := executeCommand(client, commandID, "operator-chen", oldToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(stale, http.StatusConflict, "old token must fail after handover"); err != nil {
		return err
	}
	if code, _ := stringField(stale.Body, "code"); code != "invalid_execution_token" {
		return fmt.Errorf("old token rejection code: expected invalid_execution_token, got %q", code)
	}

	wrongOperator, err := executeCommand(client, commandID, "operator-chen", newToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(wrongOperator, http.StatusConflict, "new token must not work for the old operator"); err != nil {
		return err
	}
	if code, _ := stringField(wrongOperator.Body, "code"); code != "operator_not_authorized" {
		return fmt.Errorf("wrong operator rejection code: expected operator_not_authorized, got %q", code)
	}

	stillApproved, err := client.get("/commands/" + commandID)
	if err != nil {
		return err
	}
	commandObject, err := objectField(stillApproved.Body, "command")
	if err != nil {
		return err
	}
	status, err = stringField(commandObject, "status")
	if err != nil {
		return err
	}
	if status != "approved" {
		return fmt.Errorf("failed attempts must not change command status: got %q", status)
	}

	secondHandover, err := handoverCommand(client, commandID, "supervisor-gao", "operator-shan", "连续接班")
	if err != nil {
		return err
	}
	if err := requireStatus(secondHandover, http.StatusOK, "successive handover while still approved"); err != nil {
		return err
	}
	secondCommand, err := objectField(secondHandover.Body, "command")
	if err != nil {
		return err
	}
	secondToken, err := stringField(secondCommand, "execution_token")
	if err != nil {
		return err
	}
	if secondToken == newToken {
		return fmt.Errorf("successive handover must rotate the execution token")
	}
	revoked, err := executeCommand(client, commandID, "operator-bai", newToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(revoked, http.StatusConflict, "previous handover token must be revoked"); err != nil {
		return err
	}

	finalExecute, err := executeCommand(client, commandID, "operator-shan", secondToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(finalExecute, http.StatusOK, "incoming operator executes with new token"); err != nil {
		return err
	}

	return nil
}

func verifyTerminalCommandsCannotTransfer(client *apiClient, terminal string) error {
	commandID, token, err := approveFixtureCommand(client, "gate-handover-"+terminal, 1.5)
	if err != nil {
		return err
	}

	executed, err := executeCommand(client, commandID, "operator-chen", token, terminal == "aborted")
	if err != nil {
		return err
	}
	if err := requireStatus(executed, http.StatusOK, "drive command to "+terminal); err != nil {
		return err
	}
	executedCommand, err := objectField(executed.Body, "command")
	if err != nil {
		return err
	}
	status, err := stringField(executedCommand, "status")
	if err != nil {
		return err
	}
	if status != terminal {
		return fmt.Errorf("fixture command terminal status: expected %s, got %q", terminal, status)
	}

	handover, err := handoverCommand(client, commandID, "supervisor-gao", "operator-bai", "终态交接必须拒绝")
	if err != nil {
		return err
	}
	if err := requireStatus(handover, http.StatusConflict, terminal+" command cannot be transferred"); err != nil {
		return err
	}
	if code, _ := stringField(handover.Body, "code"); code != "command_not_transferable" {
		return fmt.Errorf("terminal handover code: expected command_not_transferable, got %q", code)
	}
	return nil
}

func verifyNonApprovedCommandsCannotTransfer(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-handover-pending", "闸门-gate-handover-pending", 4, 0.25); err != nil {
		return err
	}
	pending, err := fixtureCommand(client, "gate-handover-pending", 1.75, "待复核交接", "dispatcher-zhao")
	if err != nil {
		return err
	}
	pendingID, err := stringField(pending, "id")
	if err != nil {
		return err
	}
	pendingHandover, err := handoverCommand(client, pendingID, "supervisor-gao", "operator-bai", "待复核不能交接")
	if err != nil {
		return err
	}
	if err := requireStatus(pendingHandover, http.StatusConflict, "pending command cannot be transferred"); err != nil {
		return err
	}
	if code, _ := stringField(pendingHandover.Body, "code"); code != "command_not_transferable" {
		return fmt.Errorf("pending handover code: expected command_not_transferable, got %q", code)
	}

	blocked, err := client.post("/commands/"+pendingID+"/interlock", map[string]any{
		"verdict":  "blocked",
		"verifier": "safety-he",
		"note":     "复核未通过",
	})
	if err != nil {
		return err
	}
	if err := requireStatus(blocked, http.StatusOK, "block fixture command"); err != nil {
		return err
	}
	rejectedHandover, err := handoverCommand(client, pendingID, "supervisor-gao", "operator-bai", "拒绝不能交接")
	if err != nil {
		return err
	}
	if err := requireStatus(rejectedHandover, http.StatusConflict, "rejected command cannot be transferred"); err != nil {
		return err
	}

	unknown, err := handoverCommand(client, "cmd-does-not-exist", "supervisor-gao", "operator-bai", "未知命令")
	if err != nil {
		return err
	}
	if err := requireStatus(unknown, http.StatusNotFound, "unknown command handover"); err != nil {
		return err
	}

	approvedID, _, approveErr := approveFixtureCommand(client, "gate-handover-invalid", 2.25)
	if approveErr != nil {
		return approveErr
	}
	missingSupervisor, err := handoverCommand(client, approvedID, "", "operator-bai", "缺少值班负责人")
	if err != nil {
		return err
	}
	if err := requireStatus(missingSupervisor, http.StatusBadRequest, "handover requires a shift supervisor"); err != nil {
		return err
	}
	missingOperator, err := handoverCommand(client, approvedID, "supervisor-gao", " ", "缺少接班操作员")
	if err != nil {
		return err
	}
	if err := requireStatus(missingOperator, http.StatusBadRequest, "handover requires an incoming operator"); err != nil {
		return err
	}
	return nil
}

func verifyConcurrentHandover(client *apiClient) error {
	commandID, firstToken, err := approveFixtureCommand(client, "gate-handover-conc", 2.5)
	if err != nil {
		return err
	}

	type handoverOutcome struct {
		status   int
		token    string
		operator string
	}
	const contenders = 8
	outcomes := make([]handoverOutcome, contenders)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			operator := fmt.Sprintf("operator-take-%d", i)
			result, callErr := handoverCommand(client, commandID, "supervisor-gao", operator, "并发交接")
			if callErr != nil {
				return
			}
			outcomes[i] = handoverOutcome{status: result.Status, operator: operator}
			if result.Status == http.StatusOK {
				if command, objErr := objectField(result.Body, "command"); objErr == nil {
					outcomes[i].token, _ = stringField(command, "execution_token")
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	current, err := client.get("/commands/" + commandID)
	if err != nil {
		return err
	}
	currentCommand, err := objectField(current.Body, "command")
	if err != nil {
		return err
	}
	winningOperator, err := stringField(currentCommand, "authorized_operator")
	if err != nil {
		return err
	}
	if winningOperator == "" {
		return fmt.Errorf("concurrent handover: no winning authorized operator recorded")
	}

	winningToken := ""
	attempts := 0
	for i, outcome := range outcomes {
		if outcome.status != http.StatusOK {
			return fmt.Errorf("concurrent handover %d: expected HTTP 200, got %d", i, outcome.status)
		}
		if outcome.token == "" || outcome.token == firstToken {
			return fmt.Errorf("concurrent handover %d: must return a fresh token", i)
		}
		if outcome.operator == winningOperator {
			winningToken = outcome.token
		}
		executed, execErr := executeCommand(client, commandID, outcome.operator, outcome.token, false)
		if execErr != nil {
			return execErr
		}
		switch executed.Status {
		case http.StatusOK:
			attempts++
			if outcome.operator != winningOperator {
				return fmt.Errorf("concurrent handover %d: execution succeeded for a non-winning operator", i)
			}
		case http.StatusConflict:
			// Credential was superseded by a later handover: expected for all losers.
		default:
			return fmt.Errorf("concurrent handover %d: unexpected HTTP %d", i, executed.Status)
		}
	}
	if winningToken == "" {
		return fmt.Errorf("concurrent handover: winning operator %q had no issued token", winningOperator)
	}
	if attempts != 1 {
		return fmt.Errorf("concurrent handover: expected exactly 1 executable credential, got %d", attempts)
	}

	final, err := client.get("/commands/" + commandID)
	if err != nil {
		return err
	}
	finalCommand, err := objectField(final.Body, "command")
	if err != nil {
		return err
	}
	finalStatus, err := stringField(finalCommand, "status")
	if err != nil {
		return err
	}
	if finalStatus != "completed" {
		return fmt.Errorf("concurrent handover final status: expected completed, got %q", finalStatus)
	}
	return nil
}

func verifyHandoverExecuteRace(client *apiClient) error {
	commandID, originalToken, err := approveFixtureCommand(client, "gate-handover-race", 3.0)
	if err != nil {
		return err
	}

	type raceOutcome struct {
		kind     string
		status   int
		token    string
		operator string
	}
	const contenders = 10
	outcomes := make([]raceOutcome, contenders)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				operator := fmt.Sprintf("operator-race-%d", i)
				result, callErr := handoverCommand(client, commandID, "supervisor-gao", operator, "交接与启动竞争")
				if callErr != nil {
					return
				}
				outcomes[i] = raceOutcome{kind: "handover", status: result.Status, operator: operator}
				if result.Status == http.StatusOK {
					if command, objErr := objectField(result.Body, "command"); objErr == nil {
						outcomes[i].token, _ = stringField(command, "execution_token")
					}
				}
				return
			}
			result, callErr := executeCommand(client, commandID, "operator-chen", originalToken, false)
			if callErr != nil {
				return
			}
			outcomes[i] = raceOutcome{kind: "execute", status: result.Status}
		}(i)
	}
	close(start)
	wg.Wait()

	successfulExecutes := 0
	handoverWinners := 0
	for i, outcome := range outcomes {
		if outcome.kind == "" {
			return fmt.Errorf("race contender %d produced no outcome", i)
		}
		if outcome.status != http.StatusOK && outcome.status != http.StatusConflict {
			return fmt.Errorf("race %s %d: unexpected HTTP %d", outcome.kind, i, outcome.status)
		}
		if outcome.status != http.StatusOK {
			continue
		}
		switch outcome.kind {
		case "execute":
			successfulExecutes++
		case "handover":
			handoverWinners++
			if outcome.token == "" || outcome.token == originalToken {
				return fmt.Errorf("race handover %d: must return a fresh token", i)
			}
		}
	}

	if successfulExecutes > 1 {
		return fmt.Errorf("handover/execute race: execution succeeded %d times, expected at most 1", successfulExecutes)
	}
	if successfulExecutes == 1 && handoverWinners != 0 {
		return fmt.Errorf("handover/execute race: both execute and handover succeeded, only one side may win")
	}

	current, err := client.get("/commands/" + commandID)
	if err != nil {
		return err
	}
	currentCommand, err := objectField(current.Body, "command")
	if err != nil {
		return err
	}
	currentStatus, err := stringField(currentCommand, "status")
	if err != nil {
		return err
	}

	if successfulExecutes == 1 {
		if currentStatus != "completed" {
			return fmt.Errorf("race won by execute: expected completed, got %q", currentStatus)
		}
		for i, outcome := range outcomes {
			if outcome.kind != "handover" || outcome.status != http.StatusOK {
				continue
			}
			return fmt.Errorf("race contender %d: handover unexpectedly succeeded alongside execute", i)
		}
		return nil
	}

	if currentStatus != "approved" {
		return fmt.Errorf("race won by handover: expected approved, got %q", currentStatus)
	}
	if handoverWinners == 0 {
		return fmt.Errorf("handover/execute race: neither side succeeded")
	}

	stale, err := executeCommand(client, commandID, "operator-chen", originalToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(stale, http.StatusConflict, "original credential must lose the handover/execute race"); err != nil {
		return err
	}

	finalOperator, err := stringField(currentCommand, "authorized_operator")
	if err != nil {
		return err
	}
	if finalOperator == "" {
		return fmt.Errorf("race won by handover: missing winning authorized operator")
	}
	finalToken := ""
	for _, outcome := range outcomes {
		if outcome.kind == "handover" && outcome.status == http.StatusOK && outcome.operator == finalOperator {
			finalToken = outcome.token
		}
	}
	if finalToken == "" {
		return fmt.Errorf("race won by handover: winning operator %q had no issued token", finalOperator)
	}
	for i, outcome := range outcomes {
		if outcome.kind != "handover" || outcome.status != http.StatusOK {
			continue
		}
		result, execErr := executeCommand(client, commandID, outcome.operator, outcome.token, false)
		if execErr != nil {
			return execErr
		}
		winning := outcome.operator == finalOperator && outcome.token == finalToken
		if winning {
			if result.Status != http.StatusOK {
				return fmt.Errorf("race handover %d: winning credential failed to execute (HTTP %d)", i, result.Status)
			}
			break
		}
		if result.Status != http.StatusConflict {
			return fmt.Errorf("race handover %d: superseded credential unexpectedly executed (HTTP %d)", i, result.Status)
		}
	}

	finished, err := client.get("/commands/" + commandID)
	if err != nil {
		return err
	}
	finishedCommand, err := objectField(finished.Body, "command")
	if err != nil {
		return err
	}
	finishedStatus, err := stringField(finishedCommand, "status")
	if err != nil {
		return err
	}
	if finishedStatus != "completed" {
		return fmt.Errorf("race final status: expected completed, got %q", finishedStatus)
	}
	return nil
}
