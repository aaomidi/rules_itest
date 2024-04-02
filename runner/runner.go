package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"time"

	"rules_itest/logger"
	"rules_itest/runner/topological"
	"rules_itest/svclib"
)

type ServiceSpecs = map[string]svclib.VersionedServiceSpec

type runner struct {
	ctx          context.Context
	serviceSpecs ServiceSpecs

	serviceInstances map[string]*ServiceInstance
}

func New(ctx context.Context, serviceSpecs ServiceSpecs) (*runner, error) {
	r := &runner{
		ctx:              ctx,
		serviceInstances: map[string]*ServiceInstance{},
	}
	err := r.UpdateSpecs(serviceSpecs, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *runner) StartAll() ([]topological.Task, error) {
	starter := newTopologicalStarter(r.serviceInstances)
	err := starter.Run(r.ctx)
	return starter.CriticalPath(), err
}

func (r *runner) StopAll() (map[string]*os.ProcessState, error) {
	states := make(map[string]*os.ProcessState)

	for _, serviceInstance := range r.serviceInstances {
		stopInstance(serviceInstance)
		states[serviceInstance.Label] = serviceInstance.Cmd.ProcessState
	}

	return states, nil
}

func (r *runner) GetStartDurations() map[string]time.Duration {
	durations := make(map[string]time.Duration)

	for _, serviceInstance := range r.serviceInstances {
		durations[serviceInstance.Label] = serviceInstance.startDuration
	}

	return durations
}

type updateActions struct {
	toStopLabels   []string
	toStartLabels  []string
	toReloadLabels []string
}

func computeUpdateActions(currentServices, newServices ServiceSpecs) updateActions {
	actions := updateActions{}

	// Check if existing services need a reload, a restart, or a shutdown.
	for label, service := range currentServices {
		newService, ok := newServices[label]
		if !ok {
			fmt.Println(label + " has been removed, stopping")
			actions.toStopLabels = append(actions.toStopLabels, label)
			continue
		}

		// We technically don't need a restart if the change is the list of deps.
		// But that should not be a common use case, so it's not worth the complexity.
		if !reflect.DeepEqual(service, newService) {
			fmt.Println(label + " definition or code has changed, restarting...")
			if service.HotReloadable && reflect.DeepEqual(service.ServiceSpec, newService.ServiceSpec) {
				// The only difference is the Version. Trust the service that
				// it prefers to receive the ibazel reload command.
				actions.toReloadLabels = append(actions.toReloadLabels, label)
			} else {
				actions.toStopLabels = append(actions.toStopLabels, label)
				actions.toStartLabels = append(actions.toStartLabels, label)
			}
			continue
		}
	}

	// Handle new services
	for label := range newServices {
		if _, ok := currentServices[label]; !ok {
			actions.toStartLabels = append(actions.toStartLabels, label)
		}
	}

	return actions
}

func (r *runner) UpdateSpecs(serviceSpecs ServiceSpecs, ibazelCmd []byte) error {
	updateActions := computeUpdateActions(r.serviceSpecs, serviceSpecs)

	for _, label := range updateActions.toStopLabels {
		serviceInstance := r.serviceInstances[label]
		stopInstance(serviceInstance)
		delete(r.serviceInstances, label)
	}

	for _, label := range updateActions.toStartLabels {
		var err error
		r.serviceInstances[label], err = prepareServiceInstance(r.ctx, serviceSpecs[label])
		if err != nil {
			return err
		}
	}

	for _, label := range updateActions.toReloadLabels {
		_, err := r.serviceInstances[label].Stdin.Write(ibazelCmd)
		if err != nil {
			return err
		}
	}

	r.serviceSpecs = serviceSpecs
	return nil
}

func (r *runner) UpdateSpecsAndRestart(
	serviceSpecs ServiceSpecs,
	ibazelCmd []byte,
) (
	[]topological.Task, error,
) {
	err := r.UpdateSpecs(serviceSpecs, ibazelCmd)
	if err != nil {
		return nil, err
	}
	return r.StartAll()
}

func prepareServiceInstance(ctx context.Context, s svclib.VersionedServiceSpec) (*ServiceInstance, error) {
	cmd := exec.CommandContext(ctx, s.Exe, s.Args...)
	setPgid(cmd)
	// Note, this leaks the caller's env into the service, so it's not hermetic.
	// For `bazel test`, Bazel is already sanitizing the env, so it's fine.
	// For `bazel run`, there is no expectation of hermeticity, and it can be nice to use env to control behavior.
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logger.New(s.Label+"> ", s.Color, os.Stdout)
	cmd.Stderr = logger.New(s.Label+"> ", s.Color, os.Stderr)

	// Even if a child process exits, Wait will block until the I/O pipes are closed.
	// They may have been forwarded to an orphaned child, so we disable that behavior to unblock exit.
	if s.Type == "service" {
		cmd.WaitDelay = 1
	}

	instance := &ServiceInstance{
		VersionedServiceSpec: s,
		Cmd:                  cmd,

		startErrFn: sync.OnceValue(cmd.Start),
	}

	if s.HotReloadable {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		instance.Stdin = stdin
	}
	return instance, nil
}

func stopInstance(serviceInstance *ServiceInstance) {
	killGroup(serviceInstance.Cmd.Process)
	serviceInstance.Cmd.Wait()

	for serviceInstance.Cmd.ProcessState == nil {
		time.Sleep(5 * time.Millisecond)
	}
}
