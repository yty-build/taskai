package console

// screenRect is terminal-cell geometry. Keeping layout geometry in one place
// lets rendering and pointer routing agree as the cockpit responds to width.
type screenRect struct {
	X      int
	Y      int
	Width  int
	Height int
}

func (r screenRect) contains(x, y int) bool {
	return r.Width > 0 && r.Height > 0 && x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

type taskWorkspaceMode uint8

const (
	taskWorkspaceCompact taskWorkspaceMode = iota
	taskWorkspaceStacked
	taskWorkspaceSplit
)

type taskWorkspaceLayout struct {
	Mode      taskWorkspaceMode
	Header    screenRect
	Navigator screenRect
	Preview   screenRect
	Notice    screenRect
	Footer    screenRect
}

func layoutTaskWorkspace(width, height int) taskWorkspaceLayout {
	headerHeight := min(3, max(0, height))
	footerHeight := 0
	noticeHeight := 0
	if height > headerHeight {
		footerHeight = 1
	}
	if height > headerHeight+footerHeight {
		noticeHeight = 1
	}
	bodyY := headerHeight
	bodyHeight := max(0, height-headerHeight-noticeHeight-footerHeight)
	layout := taskWorkspaceLayout{
		Header: screenRect{Width: width, Height: headerHeight},
		Notice: screenRect{Y: height - footerHeight - noticeHeight, Width: width, Height: noticeHeight},
		Footer: screenRect{Y: height - footerHeight, Width: width, Height: footerHeight},
	}
	switch {
	case width < 56:
		layout.Mode = taskWorkspaceCompact
		layout.Navigator = screenRect{Y: bodyY, Width: width, Height: bodyHeight}
	case width < 80:
		layout.Mode = taskWorkspaceStacked
		navigatorHeight := min(bodyHeight, max(6, bodyHeight/2))
		layout.Navigator = screenRect{Y: bodyY, Width: width, Height: navigatorHeight}
		layout.Preview = screenRect{Y: bodyY + navigatorHeight, Width: width, Height: bodyHeight - navigatorHeight}
	default:
		layout.Mode = taskWorkspaceSplit
		navigatorWidth := width * 38 / 100
		if width >= 120 {
			navigatorWidth = width * 32 / 100
		}
		navigatorWidth = min(42, max(30, navigatorWidth))
		layout.Navigator = screenRect{Y: bodyY, Width: navigatorWidth, Height: bodyHeight}
		layout.Preview = screenRect{X: navigatorWidth + 3, Y: bodyY, Width: width - navigatorWidth - 3, Height: bodyHeight}
	}
	return layout
}

type sessionRowHit struct {
	SessionIndex int
	Rect         screenRect
}

func (m Model) taskSessionHits(layout taskWorkspaceLayout) []sessionRowHit {
	indices, start, visible := m.taskSessionWindow(layout.Navigator.Height)
	result := make([]sessionRowHit, 0, visible)
	for position := 0; position < visible && start+position < len(indices); position++ {
		result = append(result, sessionRowHit{
			SessionIndex: indices[start+position],
			Rect: screenRect{
				X: layout.Navigator.X, Y: layout.Navigator.Y + 2 + position*2,
				Width: layout.Navigator.Width, Height: 2,
			},
		})
	}
	return result
}
