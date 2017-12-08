package hypervisor

import (
	"fmt"
	"strings"
	"time"

	hyperstartapi "github.com/hyperhq/runv/hyperstart/api/json"
	"github.com/hyperhq/runv/hyperstart/libhyperstart"
	"github.com/hyperhq/runv/hypervisor/network"
)

// states
const (
	StateRunning     = "RUNNING"
	StateTerminating = "TERMINATING"
	StateNone        = "NONE"
)

func (ctx *VmContext) timedKill(seconds int) {
	ctx.timer = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		if ctx != nil && ctx.handler != nil {
			ctx.DCtx.Kill(ctx)
		}
	})
}

func (ctx *VmContext) newContainer(id string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()

	if ctx.current != StateRunning {
		ctx.Log(DEBUG, "start container %s during %v", id, ctx.current)
		return NewNotReadyError(ctx.Id)
	}

	c, ok := ctx.containers[id]
	if ok {
		ctx.Log(TRACE, "start sending INIT_NEWCONTAINER")
		var err error
		err = ctx.hyperstart.NewContainer(c.VmSpec())
		ctx.Log(TRACE, "sent INIT_NEWCONTAINER")
		return err
	} else {
		return fmt.Errorf("container %s not exist", id)
	}
}

func (ctx *VmContext) restoreContainer(id string) (alive bool, err error) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()

	if ctx.current != StateRunning {
		ctx.Log(DEBUG, "start container %s during %v", id, ctx.current)
		return false, NewNotReadyError(ctx.Id)
	}

	c, ok := ctx.containers[id]
	if !ok {
		return false, fmt.Errorf("try to associate a container not exist in sandbox")
	}
	// FIXME do we need filter some error type? error=stopped isn't always true.
	err = ctx.hyperstart.RestoreContainer(c.VmSpec())
	if err != nil {
		ctx.Log(ERROR, "restore conatiner failed in hyperstart, mark as stopped: %v", err)
		if strings.Contains(err.Error(), "hyperstart closed") {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (ctx *VmContext) hyperstartAddInterface(id string) error {
	if inf := ctx.networks.getInterface(id); inf == nil {
		return fmt.Errorf("can't find interface whose ID is %s", id)
	} else {
		addrs := []hyperstartapi.IpAddress{}
		ipAddrs := strings.Split(inf.IpAddr, ",")
		for _, addr := range ipAddrs {
			ip, mask, err := network.IpParser(addr)
			if err != nil {
				return err
			}
			// size, _ := mask.Size()
			// addrs = append(addrs, hyperstartapi.IpAddress{ip.String(), fmt.Sprintf("%d", size)})
			maskStr := fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
			addrs = append(addrs, hyperstartapi.IpAddress{ip.String(), maskStr})
		}
		if err := ctx.hyperstart.UpdateInterface(libhyperstart.AddInf, inf.DeviceName, inf.NewName, addrs, inf.Mtu); err != nil {
			return err
		}

	}
	return nil
}

func (ctx *VmContext) hyperstartDeleteInterface(id string) error {
	if inf := ctx.networks.getInterface(id); inf == nil {
		return fmt.Errorf("can't find interface whose ID is %s", id)
	} else {
		// using new name as device name
		return ctx.hyperstart.UpdateInterface(libhyperstart.DelInf, inf.NewName, "", nil, 0)
	}
}

func (ctx *VmContext) hyperstartUpdateInterface(id string, addresses string, mtu uint64) error {
	var (
		addIP, delIP []hyperstartapi.IpAddress
	)
	inf := ctx.networks.getInterface(id)
	if inf == nil {
		return fmt.Errorf("can't find interface whose ID is %s", id)
	}

	if addresses != "" {
		addrs := strings.Split(addresses, ",")
		// TODO: currently if an IP address start with a '-',
		// we treat it as deleting an IP which is not very elegant.
		// Try to add one new field and function to handle this! @weizhang555
		for _, addr := range addrs {
			var del bool
			if addr[0] == '-' {
				del = true
				addr = addr[1:]
			}
			ip, mask, err := network.IpParser(addr)
			if err != nil {
				return err
			}
			// size, _ := mask.Size()
			// addrs = append(addrs, hyperstartapi.IpAddress{ip.String(), fmt.Sprintf("%d", size)})
			maskStr := fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])

			if del {
				delIP = append(delIP, hyperstartapi.IpAddress{ip.String(), maskStr})
			} else {
				addIP = append(addIP, hyperstartapi.IpAddress{ip.String(), maskStr})
			}
		}
	}

	if len(addIP) != 0 {
		if err := ctx.hyperstart.UpdateInterface(libhyperstart.AddIP, inf.NewName, "", addIP, 0); err != nil {
			return err
		}
	}

	if len(delIP) != 0 {
		if err := ctx.hyperstart.UpdateInterface(libhyperstart.DelIP, inf.NewName, "", delIP, 0); err != nil {
			return err
		}
	}

	if mtu > 0 {
		if err := ctx.hyperstart.UpdateInterface(libhyperstart.SetMtu, inf.NewName, "", nil, mtu); err != nil {
			return err
		}
	}
	return nil
}

func (ctx *VmContext) startPod() error {
	ctx.Log(INFO, "startPod: %#v", ctx.networks.sandboxInfo())
	err := ctx.hyperstart.StartSandbox(ctx.networks.sandboxInfo())
	if err == nil {
		ctx.Log(INFO, "pod start successfully")
		ctx.reportSuccess("Start POD success", []byte{})
	} else {
		reason := fmt.Sprintf("Start POD failed: %s", err.Error())
		ctx.reportVmFault(reason)
		ctx.Log(ERROR, reason)
	}
	return err
}

func (ctx *VmContext) shutdownVM() {
	ctx.setTimeout(10)
	err := ctx.hyperstart.DestroySandbox()
	if err == nil {
		ctx.Log(DEBUG, "POD destroyed")
		ctx.poweroffVM(false, "")
	} else {
		ctx.Log(WARNING, "Destroy pod failed: %#v", err)
		ctx.poweroffVM(true, fmt.Sprintf("Destroy pod failed: %#v", err))
		ctx.Close()
	}
}

func (ctx *VmContext) poweroffVM(err bool, msg string) {
	if err {
		ctx.Log(ERROR, "Shutting down because of an exception: ", msg)
	}
	//REFACTOR: kill directly instead of DCtx.Shutdown() and send shutdown information
	ctx.Log(INFO, "poweroff vm based on command: %s", msg)
	if ctx != nil && ctx.handler != nil {
		ctx.DCtx.Kill(ctx)
	}
}

// state machine
func unexpectedEventHandler(ctx *VmContext, ev VmEvent, state string) {
	switch ev.Event() {
	case COMMAND_SHUTDOWN,
		COMMAND_RELEASE,
		COMMAND_PAUSEVM:
		ctx.reportUnexpectedRequest(ev, state)
	default:
		ctx.Log(WARNING, "got unexpected event during ", state)
	}
}

func stateRunning(ctx *VmContext, ev VmEvent) {
	switch ev.Event() {
	case COMMAND_SHUTDOWN:
		ctx.Log(TRACE, "got shutdown command, shutting down")
		go ctx.shutdownVM()
		ctx.Become(stateTerminating, StateTerminating)
	case COMMAND_RELEASE:
		ctx.Log(TRACE, "pod is running, got release command, let VM fly")
		ctx.Become(nil, StateNone)
		ctx.reportSuccess("", nil)
	case EVENT_VM_EXIT, ERROR_VM_START_FAILED:
		ctx.Log(TRACE, "VM has exit, or not started at all (%d)", ev.Event())
		ctx.reportVmShutdown()
		ctx.Close()
	case EVENT_VM_KILL:
		ctx.Log(TRACE, "Got VM force killed message, go to cleaning up")
		ctx.reportVmShutdown()
		ctx.Close()
	case EVENT_VM_TIMEOUT: // REFACTOR: we do not set timeout for prepare devices after the refactor, then we do not need wait this event any more
		ctx.Log(ERROR, "REFACTOR: should be no time in running state at all")
	case ERROR_INIT_FAIL: // VM connection Failure
		reason := ev.(*InitFailedEvent).Reason
		ctx.Log(ERROR, reason)
		ctx.poweroffVM(true, "connection to vm broken")
		ctx.Close()
	case ERROR_INTERRUPTED:
		ctx.Log(TRACE, "Connection interrupted, quit...")
		ctx.poweroffVM(true, "connection to vm broken")
		ctx.Close()
	default:
		unexpectedEventHandler(ctx, ev, "pod running")
	}
}

func stateTerminating(ctx *VmContext, ev VmEvent) {
	switch ev.Event() {
	case EVENT_VM_EXIT:
		ctx.Log(TRACE, "Got VM shutdown event while terminating, go to cleaning up")
		ctx.reportVmShutdown()
		ctx.Close()
	case EVENT_VM_KILL:
		ctx.Log(TRACE, "Got VM force killed message, go to cleaning up")
		ctx.reportVmShutdown()
		ctx.Close()
	case COMMAND_RELEASE:
		ctx.Log(TRACE, "vm terminating, got release")
	case EVENT_VM_TIMEOUT:
		ctx.Log(WARNING, "VM did not exit in time, try to stop it")
		ctx.poweroffVM(true, "vm terminating timeout")
		ctx.Close()
	case ERROR_INTERRUPTED:
		interruptEv := ev.(*Interrupted)
		ctx.Log(TRACE, "Connection interrupted while terminating: %s", interruptEv.Reason)
	default:
		unexpectedEventHandler(ctx, ev, "terminating")
	}
}
