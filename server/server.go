package server

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/hashicorp/raft"
	"github.com/nats-io/go-nats"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	client "github.com/tylertreat/go-liftbridge/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/tylertreat/liftbridge/server/proto"
)

type Server struct {
	config             *Config
	listener           net.Listener
	nc                 *nats.Conn
	ncRaft             *nats.Conn
	ncRepl             *nats.Conn
	logger             *log.Logger
	api                *grpc.Server
	metadata           *metadataAPI
	shutdownCh         chan struct{}
	raft               *raftNode
	leaderSub          *nats.Subscription
	startedRecovery    bool
	latestRecoveredLog *raft.Log
}

func New(config *Config) *Server {
	// Default data path to /tmp/<namespace>/<server-id> if not set.
	if config.DataPath == "" {
		config.DataPath = filepath.Join(
			"/tmp", config.Clustering.Namespace, config.Clustering.ServerID)
	}
	logger := log.New()
	logger.SetLevel(log.Level(config.LogLevel))
	s := &Server{
		config:     config,
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}
	s.metadata = newMetadataAPI(s)
	return s
}

func (s *Server) Start() error {
	hp := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	l, err := net.Listen("tcp", hp)
	if err != nil {
		return errors.Wrap(err, "failed starting listener")
	}
	s.listener = l

	// NATS connection used for stream data.
	nc, err := s.config.NATS.Connect()
	if err != nil {
		s.Stop()
		return errors.Wrap(err, "failed to connect to NATS")
	}
	s.nc = nc

	// NATS connection used for Raft metadata replication.
	ncr, err := s.config.NATS.Connect()
	if err != nil {
		s.Stop()
		return errors.Wrap(err, "failed to connect to NATS")
	}
	s.ncRaft = ncr

	// NATS connection used for stream replication.
	ncRepl, err := s.config.NATS.Connect()
	if err != nil {
		s.Stop()
		return errors.Wrap(err, "failed to connect to NATS")
	}
	s.ncRepl = ncRepl

	s.logger.Infof("Server ID: %s", s.config.Clustering.ServerID)
	s.logger.Infof("Namespace: %s", s.config.Clustering.Namespace)
	s.logger.Infof("Starting server on %s...",
		net.JoinHostPort(s.config.Host, strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))

	if err := s.startMetadataRaft(); err != nil {
		s.Stop()
		return errors.Wrap(err, "failed to start Raft node")
	}

	api := grpc.NewServer()
	s.api = api
	client.RegisterAPIServer(api, &apiServer{s})
	return api.Serve(s.listener)
}

func (s *Server) finishedRecovery() (int, error) {
	count := 0
	for _, stream := range s.metadata.GetStreams() {
		recovered, err := stream.StartRecovered()
		if err != nil {
			return 0, err
		}
		if recovered {
			count++
		}
	}
	return count, nil
}

func (s *Server) startMetadataRaft() error {
	if err := s.setupMetadataRaft(); err != nil {
		return err
	}
	node := s.raft

	go func() {
		for {
			select {
			case isLeader := <-node.notifyCh:
				if isLeader {
					if err := s.leadershipAcquired(); err != nil {
						s.logger.Errorf("Error on metadata leadership acquired: %v", err)
						switch {
						case err == raft.ErrRaftShutdown:
							// Node shutdown, just return.
							return
						case err == raft.ErrLeadershipLost:
							// Node lost leadership, continue loop.
							continue
						default:
							// TODO: probably step down as leader?
							panic(err)
						}
					}
				} else {
					if err := s.leadershipLost(); err != nil {
						s.logger.Errorf("Error on metadata leadership lost: %v", err)
					}
				}
			case <-s.shutdownCh:
				return
			}
		}
	}()

	return nil
}

// leadershipAcquired should be called when this node is elected leader.
func (s *Server) leadershipAcquired() error {
	s.logger.Infof("Server became metadata leader, performing leader promotion actions")

	// Use a barrier to ensure all preceding operations are applied to the FSM.
	if err := s.raft.Barrier(0).Error(); err != nil {
		return err
	}

	// Subscribe to leader NATS subject for propagated requests.
	sub, err := s.nc.Subscribe(s.getPropagateInbox(), s.handlePropagatedRequest)
	if err != nil {
		return err
	}
	s.leaderSub = sub

	atomic.StoreInt64(&s.raft.leader, 1)
	return nil
}

// leadershipLost should be called when this node loses leadership.
func (s *Server) leadershipLost() error {
	s.logger.Warn("Server lost metadata leadership, performing leader stepdown actions")

	// Unsubscribe from leader NATS subject for propagated requests.
	if s.leaderSub != nil {
		if err := s.leaderSub.Unsubscribe(); err != nil {
			return err
		}
		s.leaderSub = nil
	}

	s.metadata.LostLeadership()
	atomic.StoreInt64(&s.raft.leader, 0)
	return nil
}

func (s *Server) isLeader() bool {
	return atomic.LoadInt64(&s.raft.leader) == 1
}

func (s *Server) getPropagateInbox() string {
	return fmt.Sprintf("%s.%s.raft.propagate",
		s.config.Clustering.Namespace, metadataRaftName)
}

func (s *Server) handlePropagatedRequest(m *nats.Msg) {
	if m.Reply == "" {
		s.logger.Warn("Invalid propagated request: no reply inbox")
		return
	}
	req := &proto.PropagatedRequest{}
	if err := req.Unmarshal(m.Data); err != nil {
		s.logger.Warnf("Invalid propagated request: %v", err)
		return
	}
	switch req.Op {
	case proto.Op_CREATE_STREAM:
		resp := &proto.PropagatedResponse{
			Op:               req.Op,
			CreateStreamResp: &client.CreateStreamResponse{},
		}
		if err := s.metadata.CreateStream(context.Background(), req.CreateStreamOp); err != nil {
			resp.Error = &proto.Error{Code: uint32(err.Code()), Msg: err.Message()}
		}
		data, err := resp.Marshal()
		if err != nil {
			panic(err)
		}
		s.nc.Publish(m.Reply, data)
	case proto.Op_SHRINK_ISR:
		resp := &proto.PropagatedResponse{
			Op: req.Op,
		}
		if err := s.metadata.ShrinkISR(context.Background(), req.ShrinkISROp); err != nil {
			resp.Error = &proto.Error{Code: uint32(err.Code()), Msg: err.Message()}
		}
		data, err := resp.Marshal()
		if err != nil {
			panic(err)
		}
		s.nc.Publish(m.Reply, data)
	case proto.Op_REPORT_LEADER:
		resp := &proto.PropagatedResponse{
			Op: req.Op,
		}
		if err := s.metadata.ReportLeader(context.Background(), req.ReportLeaderOp); err != nil {
			resp.Error = &proto.Error{Code: uint32(err.Code()), Msg: err.Message()}
		}
		data, err := resp.Marshal()
		if err != nil {
			panic(err)
		}
		s.nc.Publish(m.Reply, data)
	case proto.Op_EXPAND_ISR:
		resp := &proto.PropagatedResponse{
			Op: req.Op,
		}
		if err := s.metadata.ExpandISR(context.Background(), req.ExpandISROp); err != nil {
			resp.Error = &proto.Error{Code: uint32(err.Code()), Msg: err.Message()}
		}
		data, err := resp.Marshal()
		if err != nil {
			panic(err)
		}
		s.nc.Publish(m.Reply, data)
	default:
		s.logger.Warnf("Unknown propagated request operation: %s", req.Op)
	}
}

func (s *Server) Stop() error {
	close(s.shutdownCh)
	if s.api != nil {
		s.api.Stop()
	}

	if s.listener != nil {
		s.listener.Close()
	}

	if s.metadata != nil {
		if err := s.metadata.Reset(); err != nil {
			return err
		}
	}

	if s.raft != nil {
		if err := s.raft.shutdown(); err != nil {
			return err
		}
	}

	if s.nc != nil {
		s.nc.Close()
	}
	if s.ncRaft != nil {
		s.ncRaft.Close()
	}
	if s.ncRepl != nil {
		s.ncRepl.Close()
	}

	return nil
}
