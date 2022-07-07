package remote

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/scripthaus-dev/mshell/pkg/base"
	"github.com/scripthaus-dev/mshell/pkg/packet"
	"github.com/scripthaus-dev/mshell/pkg/shexec"
	"github.com/scripthaus-dev/sh2-server/pkg/scpacket"
	"github.com/scripthaus-dev/sh2-server/pkg/sstore"
)

const RemoteTypeMShell = "mshell"
const DefaultTermRows = 25
const DefaultTermCols = 80
const DefaultTerm = "xterm-256color"

const (
	StatusInit         = "init"
	StatusConnected    = "connected"
	StatusDisconnected = "disconnected"
	StatusError        = "error"
)

var GlobalStore *Store

type Store struct {
	Lock *sync.Mutex
	Map  map[string]*MShellProc // key=remoteid
}

type RemoteState struct {
	RemoteType   string              `json:"remotetype"`
	RemoteId     string              `json:"remoteid"`
	RemoteName   string              `json:"remotename"`
	Status       string              `json:"status"`
	DefaultState *sstore.RemoteState `json:"defaultstate"`
}

type MShellProc struct {
	Lock   *sync.Mutex
	Remote *sstore.RemoteType

	// runtime
	Status     string
	ServerProc *shexec.ClientProc
	Err        error
}

func LoadRemotes(ctx context.Context) error {
	GlobalStore = &Store{
		Lock: &sync.Mutex{},
		Map:  make(map[string]*MShellProc),
	}
	allRemotes, err := sstore.GetAllRemotes(ctx)
	if err != nil {
		return err
	}
	for _, remote := range allRemotes {
		msh := MakeMShell(remote)
		GlobalStore.Map[remote.RemoteId] = msh
		if remote.AutoConnect {
			go msh.Launch()
		}
	}
	return nil
}

func GetRemoteByName(name string) *MShellProc {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	for _, msh := range GlobalStore.Map {
		if msh.Remote.RemoteName == name {
			return msh
		}
	}
	return nil
}

func GetRemoteById(remoteId string) *MShellProc {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	return GlobalStore.Map[remoteId]
}

func GetAllRemoteState() []RemoteState {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()

	var rtn []RemoteState
	for _, proc := range GlobalStore.Map {
		state := RemoteState{
			RemoteType: proc.Remote.RemoteType,
			RemoteId:   proc.Remote.RemoteId,
			RemoteName: proc.Remote.RemoteName,
			Status:     proc.Status,
		}
		if proc.ServerProc != nil && proc.ServerProc.InitPk != nil {
			state.DefaultState = &sstore.RemoteState{Cwd: proc.ServerProc.InitPk.HomeDir}
		}
		rtn = append(rtn, state)
	}
	return rtn
}

func MakeMShell(r *sstore.RemoteType) *MShellProc {
	rtn := &MShellProc{Lock: &sync.Mutex{}, Remote: r, Status: StatusInit}
	return rtn
}

func (msh *MShellProc) Launch() {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()

	msPath, err := base.GetMShellPath()
	if err != nil {
		msh.Status = StatusError
		msh.Err = err
		return
	}
	ecmd := exec.Command(msPath, "--server")
	cproc, err := shexec.MakeClientProc(ecmd)
	if err != nil {
		msh.Status = StatusError
		msh.Err = err
		return
	}
	msh.ServerProc = cproc
	fmt.Printf("START MAKECLIENTPROC: %#v\n", msh.ServerProc.InitPk)
	msh.Status = StatusConnected
	go func() {
		exitErr := cproc.Cmd.Wait()
		exitCode := shexec.GetExitCode(exitErr)
		msh.WithLock(func() {
			if msh.Status == StatusConnected {
				msh.Status = StatusDisconnected
			}
		})
		fmt.Printf("[error] RUNNER PROC EXITED code[%d]\n", exitCode)
	}()
	go msh.ProcessPackets()
	return
}

func (msh *MShellProc) IsConnected() bool {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	return msh.Status == StatusConnected
}

func RunCommand(ctx context.Context, pk *scpacket.FeCommandPacketType, cmdId string) (*packet.CmdStartPacketType, error) {
	msh := GetRemoteById(pk.RemoteState.RemoteId)
	if msh == nil {
		return nil, fmt.Errorf("no remote id=%s found", pk.RemoteState.RemoteId)
	}
	if !msh.IsConnected() {
		return nil, fmt.Errorf("remote '%s' is not connected", msh.Remote.RemoteName)
	}
	runPacket := packet.MakeRunPacket()
	runPacket.ReqId = uuid.New().String()
	runPacket.CK = base.MakeCommandKey(pk.SessionId, cmdId)
	runPacket.Cwd = pk.RemoteState.Cwd
	runPacket.Env = nil
	runPacket.UsePty = true
	runPacket.TermOpts = &packet.TermOpts{Rows: DefaultTermRows, Cols: DefaultTermCols, Term: DefaultTerm}
	runPacket.Command = strings.TrimSpace(pk.CmdStr)
	fmt.Printf("RUN-CMD> %s\n", runPacket.CK)
	msh.ServerProc.Output.RegisterRpc(runPacket.ReqId)
	err := shexec.SendRunPacketAndRunData(ctx, msh.ServerProc.Input, runPacket)
	if err != nil {
		return nil, fmt.Errorf("sending run packet to remote: %w", err)
	}
	rtnPk := msh.ServerProc.Output.WaitForResponse(ctx, runPacket.ReqId)
	if startPk, ok := rtnPk.(*packet.CmdStartPacketType); ok {
		return startPk, nil
	}
	if respPk, ok := rtnPk.(*packet.ResponsePacketType); ok {
		if respPk.Error != "" {
			return nil, errors.New(respPk.Error)
		}
	}
	return nil, fmt.Errorf("invalid response received from server for run packet: %s", packet.AsString(rtnPk))
}

func (msh *MShellProc) PacketRpc(ctx context.Context, pk packet.RpcPacketType) (*packet.ResponsePacketType, error) {
	if !msh.IsConnected() {
		return nil, fmt.Errorf("runner is not connected")
	}
	if pk == nil {
		return nil, fmt.Errorf("PacketRpc passed nil packet")
	}
	reqId := pk.GetReqId()
	msh.ServerProc.Output.RegisterRpc(reqId)
	defer msh.ServerProc.Output.UnRegisterRpc(reqId)
	err := msh.ServerProc.Input.SendPacketCtx(ctx, pk)
	if err != nil {
		return nil, err
	}
	rtnPk := msh.ServerProc.Output.WaitForResponse(ctx, reqId)
	if rtnPk == nil {
		return nil, ctx.Err()
	}
	if respPk, ok := rtnPk.(*packet.ResponsePacketType); ok {
		return respPk, nil
	}
	return nil, fmt.Errorf("invalid response packet received: %s", packet.AsString(rtnPk))
}

func (runner *MShellProc) WithLock(fn func()) {
	runner.Lock.Lock()
	defer runner.Lock.Unlock()
	fn()
}

func (runner *MShellProc) ProcessPackets() {
	defer runner.WithLock(func() {
		if runner.Status == StatusConnected {
			runner.Status = StatusDisconnected
		}
	})
	for pk := range runner.ServerProc.Output.MainCh {
		fmt.Printf("MSH> %s | %#v\n", packet.AsString(pk), pk)
		if pk.GetType() == packet.DataPacketStr {
			dataPacket := pk.(*packet.DataPacketType)
			fmt.Printf("data %s fd=%d len=%d eof=%v err=%v\n", dataPacket.CK, dataPacket.FdNum, packet.B64DecodedLen(dataPacket.Data64), dataPacket.Eof, dataPacket.Error)
			continue
		}
		if pk.GetType() == packet.CmdDataPacketStr {
			dataPacket := pk.(*packet.CmdDataPacketType)
			fmt.Printf("cmd-data %s pty=%d run=%d\n", dataPacket.CK, dataPacket.PtyDataLen, dataPacket.RunDataLen)
			continue
		}
		if pk.GetType() == packet.CmdDonePacketStr {
			donePacket := pk.(*packet.CmdDonePacketType)
			fmt.Printf("cmd-done %s\n", donePacket.CK)
			continue
		}
		if pk.GetType() == packet.MessagePacketStr {
			msgPacket := pk.(*packet.MessagePacketType)
			fmt.Printf("# %s\n", msgPacket.Message)
			continue
		}
		if pk.GetType() == packet.RawPacketStr {
			rawPacket := pk.(*packet.RawPacketType)
			fmt.Printf("stderr> %s\n", rawPacket.Data)
			continue
		}
		fmt.Printf("runner-packet: %v\n", pk)
	}
}
