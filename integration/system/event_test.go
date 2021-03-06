package system // import "github.com/docker/docker/integration/system"

import (
	"context"
	"testing"

	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/integration/util/request"
	"github.com/stretchr/testify/require"
)

func TestEvents(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := request.NewAPIClient(t)

	container, err := client.ContainerCreate(ctx,
		&container.Config{
			Image:      "busybox",
			Tty:        true,
			WorkingDir: "/root",
			Cmd:        strslice.StrSlice([]string{"top"}),
		},
		&container.HostConfig{},
		&network.NetworkingConfig{},
		"foo",
	)
	require.NoError(t, err)
	err = client.ContainerStart(ctx, container.ID, types.ContainerStartOptions{})
	require.NoError(t, err)

	id, err := client.ContainerExecCreate(ctx, container.ID,
		types.ExecConfig{
			Cmd: strslice.StrSlice([]string{"echo", "hello"}),
		},
	)
	require.NoError(t, err)

	filters := filters.NewArgs(
		filters.Arg("container", container.ID),
		filters.Arg("event", "exec_die"),
	)
	msg, errors := client.Events(ctx, types.EventsOptions{
		Filters: filters,
	})

	err = client.ContainerExecStart(ctx, id.ID,
		types.ExecStartCheck{
			Detach: true,
			Tty:    false,
		},
	)
	require.NoError(t, err)

	select {
	case m := <-msg:
		require.Equal(t, m.Type, "container")
		require.Equal(t, m.Actor.ID, container.ID)
		require.Equal(t, m.Action, "exec_die")
		require.Equal(t, m.Actor.Attributes["execID"], id.ID)
		require.Equal(t, m.Actor.Attributes["exitCode"], "0")
	case err = <-errors:
		t.Fatal(err)
	case <-time.After(time.Second * 3):
		t.Fatal("timeout hit")
	}

}
