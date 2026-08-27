package console

import "strings"

type actionID string

const (
	actionOpen    actionID = "open"
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
	{ID: actionOpen, Key: "enter", Label: "Open Session", FooterLabel: "Open", Description: "Attach this disposable viewer to the Session Channel"},
	{ID: actionMessage, Key: "p", Label: "Message Session", FooterLabel: "Message", Description: "Open the Session Channel with the message composer focused"},
	{ID: actionNew, Key: "n", Label: "New Session", FooterLabel: "New Session", Description: "Add another agent Session to this Task"},
	{ID: actionCancel, Key: "c", Label: "Cancel turn", FooterLabel: "Cancel", Description: "Request cancellation of only the selected active provider turn"},
	{ID: actionResume, Key: "u", Label: "Resume worker", FooterLabel: "Resume", Description: "Exact-resume a stopped Session with a saved provider identity"},
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

func filteredTaskActions(query string, allowResume bool) []consoleAction {
	query = strings.ToLower(strings.TrimSpace(query))
	var result []consoleAction
	for _, action := range taskActions {
		if action.ID == actionResume && !allowResume {
			continue
		}
		if query == "" {
			result = append(result, action)
			continue
		}
		haystack := strings.ToLower(action.Key + " " + action.Label + " " + action.Description)
		if strings.Contains(haystack, query) {
			result = append(result, action)
		}
	}
	return result
}
