package console

import "strings"

type actionID string

const (
	actionNative  actionID = "native"
	actionACP     actionID = "acp"
	actionMessage actionID = "message"
	actionNew     actionID = "new"
	actionCancel  actionID = "cancel"
	actionResume  actionID = "resume"
	actionStop    actionID = "stop"
	actionFilter  actionID = "filter"
	actionPalette actionID = "palette"
	actionHelp    actionID = "help"
	actionDetach  actionID = "detach"
)

type consoleAction struct {
	ID          actionID
	Key         string
	Label       string
	Description string
}

// taskActions is the single source for direct keys, help, the command palette,
// and contextual footer labels.
var taskActions = []consoleAction{
	{ID: actionNative, Key: "enter", Label: "Native harness", Description: "Resume this exact conversation in the provider's real CLI"},
	{ID: actionACP, Key: "a", Label: "ACP console", Description: "Open the structured gateway transcript and permission view"},
	{ID: actionMessage, Key: "p", Label: "Message through ACP", Description: "Open the ACP console with the message composer focused"},
	{ID: actionNew, Key: "n", Label: "New Session", Description: "Add another agent Session to this Task"},
	{ID: actionCancel, Key: "c", Label: "Cancel turn", Description: "Request cancellation of only the selected active ACP turn"},
	{ID: actionResume, Key: "u", Label: "Resume worker", Description: "Resume the selected Session worker"},
	{ID: actionStop, Key: "x", Label: "Stop worker", Description: "Stop the worker without deleting provider conversation state"},
	{ID: actionFilter, Key: "/", Label: "Filter Sessions", Description: "Filter by ID, agent, role, status, repo, or activity"},
	{ID: actionPalette, Key: ":", Label: "Command palette", Description: "Search every action available on this screen"},
	{ID: actionHelp, Key: "?", Label: "Keyboard help", Description: "Show the complete key map and interaction model"},
	{ID: actionDetach, Key: "q", Label: "Detach viewer", Description: "Close only this viewer; Sessions and turns continue"},
}

func taskActionForKey(key string) (consoleAction, bool) {
	for _, action := range taskActions {
		if action.Key == key {
			return action, true
		}
	}
	return consoleAction{}, false
}

func filteredTaskActions(query string) []consoleAction {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return append([]consoleAction(nil), taskActions...)
	}
	var result []consoleAction
	for _, action := range taskActions {
		haystack := strings.ToLower(action.Key + " " + action.Label + " " + action.Description)
		if strings.Contains(haystack, query) {
			result = append(result, action)
		}
	}
	return result
}
