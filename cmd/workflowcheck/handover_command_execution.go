package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

func runHandoverCommandExecution(client *apiClient) error {
	if err := checkHandoverLifecycle(client); err != nil {
		return err
	}
	if err := checkHandoverRejectsPendingCommand(client); err != nil {
		return err
	}
	if err := checkHandoverExecuteRace(client); err != nil {
		return err
	}
	if err := checkConcurrentHandovers(client); err != nil {
		return err
	}
	if err := checkHandoverRejectsAbortedCommand(client); err != nil {
		return err
	}

	fmt.Println("handover-command-execution: authority transferred, stale token rejected, races resolved atomically")
	return nil
}

func checkHandoverLifecycle(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-north", "北侧泄洪闸", 6, 0.5); err != nil {
		return err
	}
	command, err := fixtureCommand(client, "gate-north", 3.0, "交班前调整下泄流量", "dispatcher-qian")
	if err != nil {
		return err
	}
	commandID, err := stringField(command, "id")
	if err != nil {
		return err
	}
	approved, err := fixtureClearReview(client, commandID, "safety-wu")
	if err != nil {
		return err
	}
	staleToken, err := stringField(approved, "execution_token")
	if err != nil {
		return err
	}

	missingSupervisor, err := postHandover(client, commandID, "", "operator-successor")
	if err != nil {
		return err
	}
	if err := requireStatus(missingSupervisor, http.StatusBadRequest, "reject handover without supervisor"); err != nil {
		return err
	}
	missingOperator, err := postHandover(client, commandID, "supervisor-liu", " ")
	if err != nil {
		return err
	}
	if err := requireStatus(missingOperator, http.StatusBadRequest, "reject handover without operator"); err != nil {
		return err
	}

	handedOver, err := postHandover(client, commandID, "supervisor-liu", "operator-successor")
	if err != nil {
		return err
	}
	if err := requireStatus(handedOver, http.StatusOK, "handover approved command"); err != nil {
		return err
	}
	handoverCommand, err := objectField(handedOver.Body, "command")
	if err != nil {
		return err
	}
	status, err := stringField(handoverCommand, "status")
	if err != nil {
		return err
	}
	if status != "approved" {
		return fmt.Errorf("handover status: expected approved, got %q", status)
	}
	newToken, err := stringField(handoverCommand, "execution_token")
	if err != nil {
		return err
	}
	if newToken == "" || newToken == staleToken {
		return fmt.Errorf("handover must rotate the execution token, got %q", newToken)
	}
	assignee, err := stringField(handoverCommand, "execution_assignee")
	if err != nil {
		return err
	}
	if assignee != "operator-successor" {
		return fmt.Errorf("execution assignee: expected operator-successor, got %q", assignee)
	}
	supervisor, err := stringField(handoverCommand, "handover_supervisor")
	if err != nil {
		return err
	}
	if supervisor != "supervisor-liu" {
		return fmt.Errorf("handover supervisor: expected supervisor-liu, got %q", supervisor)
	}
	if err := requireGateReserved(handedOver.Body, commandID, "handover keeps gate reserved"); err != nil {
		return err
	}

	stale, err := postExecute(client, commandID, "operator-chen", staleToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(stale, http.StatusConflict, "reject stale execution token"); err != nil {
		return err
	}
	if err := requireErrorCode(stale, "invalid_execution_token", "stale token execution"); err != nil {
		return err
	}

	gateResult, err := client.get("/gates/gate-north")
	if err != nil {
		return err
	}
	if err := requireStatus(gateResult, http.StatusOK, "read gate after stale token rejection"); err != nil {
		return err
	}
	if err := requireGateReserved(gateResult.Body, commandID, "stale token attempt keeps gate reserved"); err != nil {
		return err
	}

	executed, err := postExecute(client, commandID, "operator-successor", newToken, false)
	if err != nil {
		return err
	}
	if err := requireStatus(executed, http.StatusOK, "execute with handed-over token"); err != nil {
		return err
	}
	executedCommand, err := objectField(executed.Body, "command")
	if err != nil {
		return err
	}
	completedStatus, err := stringField(executedCommand, "status")
	if err != nil {
		return err
	}
	if completedStatus != "completed" {
		return fmt.Errorf("handed-over execution status: expected completed, got %q", completedStatus)
	}

	afterCompletion, err := postHandover(client, commandID, "supervisor-liu", "operator-late")
	if err != nil {
		return err
	}
	if err := requireStatus(afterCompletion, http.StatusConflict, "reject handover after completion"); err != nil {
		return err
	}
	return requireErrorCode(afterCompletion, "command_not_approved", "handover after completion")
}

func checkHandoverRejectsPendingCommand(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-pending", "待复核验证闸", 6, 0.5); err != nil {
		return err
	}
	command, err := fixtureCommand(client, "gate-pending", 1.25, "未复核命令交接验证", "dispatcher-qian")
	if err != nil {
		return err
	}
	commandID, err := stringField(command, "id")
	if err != nil {
		return err
	}

	result, err := postHandover(client, commandID, "supervisor-liu", "operator-successor")
	if err != nil {
		return err
	}
	if err := requireStatus(result, http.StatusConflict, "reject handover of pending command"); err != nil {
		return err
	}
	return requireErrorCode(result, "command_not_approved", "handover of pending command")
}

func checkHandoverExecuteRace(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-race", "交接竞争验证闸", 6, 0.5); err != nil {
		return err
	}
	command, err := fixtureCommand(client, "gate-race", 2.75, "交接与启动竞争验证", "dispatcher-qian")
	if err != nil {
		return err
	}
	commandID, err := stringField(command, "id")
	if err != nil {
		return err
	}
	approved, err := fixtureClearReview(client, commandID, "safety-wu")
	if err != nil {
		return err
	}
	token, err := stringField(approved, "execution_token")
	if err != nil {
		return err
	}

	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result, err := postHandover(client, commandID, "supervisor-liu", "operator-race")
		if err != nil {
			statuses <- -1
			return
		}
		statuses <- result.Status
	}()
	go func() {
		defer wg.Done()
		result, err := postExecute(client, commandID, "operator-chen", token, false)
		if err != nil {
			statuses <- -1
			return
		}
		statuses <- result.Status
	}()
	wg.Wait()
	close(statuses)

	outcomes := collectStatuses(statuses)
	if len(outcomes) != 2 || outcomes[0] != http.StatusOK || outcomes[1] != http.StatusConflict {
		return fmt.Errorf("handover/execute race: expected one 200 and one 409, got %v", outcomes)
	}
	return nil
}

func checkConcurrentHandovers(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-pool", "并发交接验证闸", 6, 0.5); err != nil {
		return err
	}
	command, err := fixtureCommand(client, "gate-pool", 4.0, "并发交接原子性验证", "dispatcher-qian")
	if err != nil {
		return err
	}
	commandID, err := stringField(command, "id")
	if err != nil {
		return err
	}
	if _, err := fixtureClearReview(client, commandID, "safety-wu"); err != nil {
		return err
	}

	type handoverOutcome struct {
		status int
		token  string
	}
	outcomes := make(chan handoverOutcome, 2)
	var wg sync.WaitGroup
	for _, operator := range []string{"operator-alpha", "operator-beta"} {
		wg.Add(1)
		go func(operator string) {
			defer wg.Done()
			result, err := postHandover(client, commandID, "supervisor-liu", operator)
			if err != nil {
				outcomes <- handoverOutcome{status: -1}
				return
			}
			token := ""
			if result.Status == http.StatusOK {
				if command, err := objectField(result.Body, "command"); err == nil {
					token, _ = stringField(command, "execution_token")
				}
			}
			outcomes <- handoverOutcome{status: result.Status, token: token}
		}(operator)
	}
	wg.Wait()
	close(outcomes)

	tokens := make([]string, 0, 2)
	for outcome := range outcomes {
		if outcome.status != http.StatusOK || outcome.token == "" {
			return fmt.Errorf("concurrent handover: expected 200 with a token, got %+v", outcome)
		}
		tokens = append(tokens, outcome.token)
	}
	if tokens[0] == tokens[1] {
		return fmt.Errorf("concurrent handovers issued the same token %q", tokens[0])
	}

	// 只有最后一次交接签发的令牌仍然有效，先到的令牌必须已失效。
	first, err := postExecute(client, commandID, "operator-alpha", tokens[0], false)
	if err != nil {
		return err
	}
	second, err := postExecute(client, commandID, "operator-beta", tokens[1], false)
	if err != nil {
		return err
	}
	executes := []int{first.Status, second.Status}
	sort.Ints(executes)
	if executes[0] != http.StatusOK || executes[1] != http.StatusConflict {
		return fmt.Errorf("concurrent handover tokens: expected exactly one live token, got %v", executes)
	}
	return nil
}

func checkHandoverRejectsAbortedCommand(client *apiClient) error {
	if _, err := fixtureGate(client, "gate-fault", "故障中止验证闸", 6, 0.5); err != nil {
		return err
	}
	command, err := fixtureCommand(client, "gate-fault", 5.0, "故障中止后交接验证", "dispatcher-qian")
	if err != nil {
		return err
	}
	commandID, err := stringField(command, "id")
	if err != nil {
		return err
	}
	approved, err := fixtureClearReview(client, commandID, "safety-wu")
	if err != nil {
		return err
	}
	token, err := stringField(approved, "execution_token")
	if err != nil {
		return err
	}

	aborted, err := postExecute(client, commandID, "operator-chen", token, true)
	if err != nil {
		return err
	}
	if err := requireStatus(aborted, http.StatusOK, "abort command via simulated fault"); err != nil {
		return err
	}

	result, err := postHandover(client, commandID, "supervisor-liu", "operator-successor")
	if err != nil {
		return err
	}
	if err := requireStatus(result, http.StatusConflict, "reject handover of aborted command"); err != nil {
		return err
	}
	return requireErrorCode(result, "command_not_approved", "handover of aborted command")
}

func postHandover(client *apiClient, commandID, supervisor, operator string) (apiResult, error) {
	return client.post("/commands/"+commandID+"/handover", map[string]any{
		"supervisor": supervisor,
		"operator":   operator,
	})
}

func postExecute(client *apiClient, commandID, operator, token string, simulateFault bool) (apiResult, error) {
	return client.post("/commands/"+commandID+"/execute", map[string]any{
		"operator":       operator,
		"token":          token,
		"simulate_fault": simulateFault,
	})
}

func requireGateReserved(body map[string]any, commandID string, operation string) error {
	gate, err := objectField(body, "gate")
	if err != nil {
		return err
	}
	status, err := stringField(gate, "status")
	if err != nil {
		return err
	}
	activeCommandID, err := stringField(gate, "active_command_id")
	if err != nil {
		return err
	}
	if status != "reserved" || activeCommandID != commandID {
		return fmt.Errorf(
			"%s: expected reserved gate for %s, got status=%q active_command_id=%q",
			operation,
			commandID,
			status,
			activeCommandID,
		)
	}
	return nil
}

func requireErrorCode(result apiResult, expected string, operation string) error {
	code, err := stringField(result.Body, "code")
	if err != nil {
		return err
	}
	if code != expected {
		return fmt.Errorf("%s: expected error code %q, got %q", operation, expected, code)
	}
	return nil
}

func collectStatuses(statuses chan int) []int {
	outcomes := make([]int, 0, 2)
	for status := range statuses {
		outcomes = append(outcomes, status)
	}
	sort.Ints(outcomes)
	return outcomes
}
