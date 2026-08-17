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
	FooterLabel string
	Description string
}

// taskActions is the single source for direct keys, help, the command palette,
// and contextual footer labels.
var taskActions = []consoleAction{
	{ID: actionNative, Key: "enter", Label: "Native CLI", FooterLabel: "Native CLI", Description: "Open this exact conversation in the provider's real CLI"},
	{ID: actionACP, Key: "a", Label: "Structured view", FooterLabel: "View", Description: "Open the gateway conversation, tools, and approvals"},
	{ID: actionMessage, Key: "p", Label: "Message in structured view", FooterLabel: "Message", Description: "Open the conversation with the message composer focused"},
	{ID: actionNew, Key: "n", Label: "New Session", FooterLabel: "New Session", Description: "Add another agent Session to this Task"},
	{ID: actionCancel, Key: "c", Label: "Cancel turn", FooterLabel: "Cancel", Description: "Request cancellation of only the selected active ACP turn"},
	{ID: actionResume, Key: "u", Label: "Resume worker", FooterLabel: "Resume", Description: "Resume the selected Session worker"},
	{ID: actionStop, Key: "x", Label: "Stop worker", FooterLabel: "Stop", Description: "Stop the worker without deleting provider conversation state"},
	{ID: actionFilter, Key: "/", Label: "Filter Sessions", FooterLabel: "Filter", Description: "Filter by ID, agent, role, status, repo, or activity"},
	{ID: actionPalette, Key: ":", Label: "Command palette", FooterLabel: "Commands", Description: "Search every action available on this screen"},
	{ID: actionHelp, Key: "?", Label: "Keyboard help", FooterLabel: "Help", Description: "Show the complete key map and interaction model"},
	{ID: actionDetach, Key: "q", Label: "Detach viewer", FooterLabel: "Detach", Description: "Close only this viewer; Sessions and turns continue"},
}

func taskActionForID(id actionID) (consoleAction, bool) {
	for _, action := range taskActions {
		if action.ID == id {
			return action, true
		}
	}
	return consoleAction{}, false
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
