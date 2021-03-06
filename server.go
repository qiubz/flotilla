package flotilla

import (
	"fmt"
	mdb "github.com/jbooth/gomdb"
	"github.com/jbooth/raft"
	raftmdb "github.com/jbooth/raft-mdb"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var (
	dialCodeRaft byte = 0
	dialCodeFlot byte = 1
)

// launches a new DB serving out of dataDir
func NewDefaultDB(peers []string, dataDir string, bindAddr string, ops map[string]Command) (DefaultOpsDB, error) {
	laddr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		return nil, err
	}
	listen, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	db, err := NewDB(
		peers,
		dataDir,
		listen,
		defaultDialer,
		ops,
		log.New(os.Stderr, "flotilla", log.LstdFlags),
	)
	if err != nil {
		return nil, err
	}

	// wrap with standard ops
	return dbOps{db}, nil
}

// Instantiates a new DB serving the ops provided, using the provided dataDir and listener
// If Peers is empty, we start as the sole leader.  Otherwise, connect to the existing leader.
func NewDB(
	peers []string,
	dataDir string,
	listen net.Listener,
	dialer func(string, time.Duration) (net.Conn, error),
	commands map[string]Command,
	lg *log.Logger) (DB, error) {
	raftDir := dataDir + "/raft"
	mdbDir := dataDir + "/mdb"
	// make sure dirs exist
	if err := os.MkdirAll(raftDir, 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	commandsForStateMachine := defaultCommands()
	for cmd, cmdExec := range commands {
		_, ok := commandsForStateMachine[cmd]
		if ok {
			lg.Printf("WARNING overriding command %s with user-defined command", cmd)
		}
		commandsForStateMachine[cmd] = cmdExec
	}
	state, err := newFlotillaState(
		mdbDir,
		commandsForStateMachine,
		listen.Addr().String(),
		lg,
	)
	if err != nil {
		return nil, err
	}
	streamLayers, err := NewMultiStream(listen, dialer, listen.Addr(), lg, dialCodeRaft, dialCodeFlot)
	if err != nil {
		return nil, err
	}
	// start raft server
	raft, err := newRaft(peers, raftDir, streamLayers[dialCodeRaft], state, lg)
	if err != nil {
		return nil, err
	}
	s := &server{
		raft:       raft,
		state:      state,
		peers:      peers,
		rpcLayer:   streamLayers[dialCodeFlot],
		leaderLock: new(sync.Mutex),
		leaderConn: nil,
		lg:         lg,
	}
	// serve followers
	go s.serveFollowers()
	return s, nil
}

type server struct {
	raft       *raft.Raft
	state      *flotillaState
	peers      []string
	rpcLayer   raft.StreamLayer
	leaderLock *sync.Mutex
	leaderConn *connToLeader
	lg         *log.Logger
}

func newRaft(peers []string, path string, streams raft.StreamLayer, state raft.FSM, lg *log.Logger) (*raft.Raft, error) {
	// Create the MDB store for logs and stable storage, retain up to 8gb
	store, err := raftmdb.NewMDBStoreWithSize(path, 8*1024*1024*1024)
	if err != nil {
		return nil, err
	}

	// Create the snapshot store
	snapshots, err := raft.NewFileSnapshotStoreLog(path, 1, lg)
	if err != nil {
		store.Close()
		return nil, err
	}

	// Create a transport layer
	trans := raft.NewNetworkTransportLog(streams, 3, 10*time.Second, lg)

	// Setup the peer store
	peerAddrs := make([]net.Addr, len(peers), len(peers))
	for idx, p := range peers {
		peerAddrs[idx], err = net.ResolveTCPAddr("tcp", p)
		if err != nil {
			return nil, err
		}
	}
	raftPeers := raft.NewJSONPeers(path, trans)
	if err = raftPeers.SetPeers(peerAddrs); err != nil {
		return nil, err
	}
	// Ensure local host is always included
	peerAddrs, err = raftPeers.Peers()
	if err != nil {
		store.Close()
		return nil, err
	}
	if !raft.PeerContained(peerAddrs, trans.LocalAddr()) {
		return nil, fmt.Errorf("Localhost %s not included in peers %+v", trans.LocalAddr().String(), peers)
	}

	// Setup the Raft server
	raftCfg := raft.DefaultConfig()
	if len(peers) == 1 {
		raftCfg.EnableSingleNode = true
	}
	raft, err := raft.NewRaft(raftCfg, state, store, store,
		snapshots, raftPeers, trans)
	if err != nil {
		store.Close()
		trans.Close()
		return nil, err
	}
	// wait until we've identified some valid leader
	timeout := time.Now().Add(1 * time.Minute)
	for {
		leader := raft.Leader()
		lg.Printf("server.newRaft Identified leader %s from host %s\n", leader, trans.LocalAddr().String())
		if leader != nil {
			break
		} else {
			time.Sleep(1 * time.Second)
			if time.Now().After(timeout) {
				return nil, fmt.Errorf("Timed out with no leader elected after 1 minute!")
			}
		}
	}
	return raft, nil
}

func (s *server) serveFollowers() {
	for {
		conn, err := s.rpcLayer.Accept()
		if err != nil {
			s.lg.Printf("ERROR accepting from %s : %s", s.rpcLayer.Addr().String(), err)
			return
		}
		go serveFollower(s.lg, conn, s)
	}
}

// only removes if leader, otherwise returns nil
func (s *server) RemovePeer(deadPeer net.Addr) error {
	if s.IsLeader() {
		return s.raft.RemovePeer(deadPeer).Error()
	} else {
		return nil
	}
}

// returns addr of leader
func (s *server) Leader() net.Addr {
	return s.raft.Leader()
}

// return if we are leader
func (s *server) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

var commandTimeout = 1 * time.Minute

// public API, executes a command on leader, returns chan which will
// block until command has been replicated to our local replica
func (s *server) Command(cmd string, args [][]byte) <-chan Result {

	if s.IsLeader() {
		cb := s.state.newCommand()
		cmdBytes := bytesForCommand(cb.originAddr, cb.reqNo, cmd, args)
		s.raft.Apply(cmdBytes, commandTimeout)
		return cb.result
	}
	// couldn't exec as leader, fallback to forwarding
	cb, err := s.dispatchToLeader(cmd, args)
	if err != nil {
		if cb != nil {
			cb.cancel()
		}
		ret := make(chan Result, 1)
		ret <- Result{nil, err}
		return ret
	}
	return cb.result
}

// checks connection state and dispatches the task to leader
// returns a callback registered with our state machine
func (s *server) dispatchToLeader(cmd string, args [][]byte) (*commandCallback, error) {
	s.leaderLock.Lock()
	defer s.leaderLock.Unlock()
	var err error
	if s.leaderConn == nil || s.Leader() == nil || s.Leader().String() != s.leaderConn.remoteAddr().String() {
		if s.leaderConn != nil {
			s.lg.Printf("Leader changed, reconnecting, was: %s, now %s", s.leaderConn.remoteAddr(), s.Leader())
		}

		// reconnect
		if s.leaderConn != nil {
			s.leaderConn.c.Close()
		}
		newConn, err := s.rpcLayer.Dial(s.Leader().String(), 1*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("Couldn't connect to leader at %s", s.Leader().String())
		}
		s.leaderConn, err = newConnToLeader(newConn, s.rpcLayer.Addr().String(), s.lg)
		if err != nil {
			s.lg.Printf("Got error connecting to leader %s from follower %s : %s", s.Leader().String(), s.rpcLayer.Addr().String(), err)
			return nil, err
		}
	}
	cb := s.state.newCommand()
	err = s.leaderConn.forwardCommand(cb, cmd, args)
	if err != nil {
		cb.cancel()
		return nil, err
	}
	return cb, nil
}

func (s *server) Read() (*mdb.Txn, error) {
	return s.state.ReadTxn()
}

func (s *server) Rsync() error {
	resultCh := s.Command("Noop", [][]byte{})
	result := <-resultCh
	return result.Err
}
func (s *server) Close() error {
	f := s.raft.Shutdown()
	return f.Error()
}
