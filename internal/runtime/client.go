package runtime

import (
	"context"
	"sync"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

// ClientAcceptor is the narrow transport composition seam. Opening a client
// registers viewer state only; it does not launch, probe, authenticate,
// resume, replay, attach, or inspect provider state.
type ClientAcceptor interface {
	OpenClient(string) (ClientPort, error)
}

var _ ClientAcceptor = (*Supervisor)(nil)

type localClient struct {
	supervisor *Supervisor
	viewer     *viewer
	closeOnce  sync.Once
}

var _ ClientPort = (*localClient)(nil)

func (supervisor *Supervisor) OpenClient(id string) (ClientPort, error) {
	if supervisor == nil {
		return nil, ErrInvalidRequest
	}
	registered, err := supervisor.registerViewer(id)
	if err != nil {
		return nil, err
	}
	return &localClient{supervisor: supervisor, viewer: registered}, nil
}

func (client *localClient) available() error {
	if client == nil || client.supervisor == nil || client.viewer == nil {
		return ErrClosed
	}
	select {
	case <-client.viewer.done:
		return ErrClosed
	default:
		return nil
	}
}

func (client *localClient) Snapshot(ctx context.Context) (Snapshot, error) {
	if ctx == nil || ctx.Err() != nil {
		return Snapshot{}, ErrCancelled
	}
	if err := client.available(); err != nil {
		return Snapshot{}, err
	}
	return client.supervisor.snapshot(client.viewer.id)
}

func (client *localClient) AddTask(ctx context.Context, title string) (task.ID, error) {
	if err := client.available(); err != nil {
		return "", err
	}
	return client.supervisor.addTask(ctx, client.viewer.id, title)
}

func (client *localClient) AddManager(
	ctx context.Context,
	taskID task.ID,
	providerID settings.ProviderID,
	label string,
) (session.ID, error) {
	if err := client.available(); err != nil {
		return "", err
	}
	return client.supervisor.addManager(ctx, client.viewer.id, taskID, providerID, label)
}

func (client *localClient) SetEnabled(ctx context.Context, id settings.ProviderID, enabled bool) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.setEnabled(ctx, client.viewer.id, id, enabled)
}

func (client *localClient) Authenticate(ctx context.Context, id settings.ProviderID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.authenticate(ctx, client.viewer.id, id)
}

func (client *localClient) Recheck(ctx context.Context, id settings.ProviderID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.recheck(ctx, client.viewer.id, id)
}

func (client *localClient) Attach(ctx context.Context, id task.ID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.attach(ctx, client.viewer.id, id)
}

func (client *localClient) Detach(ctx context.Context, id task.ID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.detach(ctx, client.viewer.id, id)
}

func (client *localClient) Send(ctx context.Context, id session.ID, prompt string) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.send(ctx, client.viewer.id, id, prompt)
}

func (client *localClient) Resume(ctx context.Context, id session.ID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.resume(ctx, client.viewer.id, id)
}

func (client *localClient) Stop(ctx context.Context, id session.ID) error {
	if err := client.available(); err != nil {
		return err
	}
	return client.supervisor.stop(ctx, client.viewer.id, id)
}

func (client *localClient) Next(ctx context.Context, cursor Cursor) (Event, error) {
	if err := client.available(); err != nil {
		return Event{}, err
	}
	return client.supervisor.nextEvent(ctx, client.viewer, cursor)
}

func (client *localClient) Close() error {
	if client == nil {
		return nil
	}
	client.closeOnce.Do(func() {
		if client.supervisor != nil && client.viewer != nil {
			client.supervisor.unregisterViewer(client.viewer.id, client.viewer)
		}
	})
	return nil
}
