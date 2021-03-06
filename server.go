package accord

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/qvntm/accord/pb"
)

const (
	secretKey     = "secret"
	tokenDuration = 15 * time.Minute
)

type AccordServer struct {
	authServer      *AuthServer
	authInterceptor *ServerAuthInterceptor
	listener        net.Listener
	mutex           sync.RWMutex
	channels        map[uint64]*ServerChannel
	jwtManager      *JWTManager
}

func NewAccordServer() *AccordServer {
	authServer := NewAuthServer()
	return &AccordServer{
		authServer:      authServer,
		authInterceptor: NewServerAuthInterceptor(authServer.JWTManager()),
		channels:        make(map[uint64]*ServerChannel),
		jwtManager:      NewJWTManager(secretKey, tokenDuration),
	}
}

// LoadChannels loads channels from the persistent storage
func (s *AccordServer) LoadChannels() error {
	return fmt.Errorf("unimplemented")
}

// LoadUsers loads channels from the persistent storage
func (s *AccordServer) LoadUsers() error {
	return fmt.Errorf("unimplemented")
}

func getUsernameFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("metadata is not provided")
	}

	values := md["username"]
	if len(values) == 0 {
		return "", fmt.Errorf("there is no username key in metadata")
	}

	return values[0], nil
}

// AddChannel creates a new channel with given parameters. The user who created the channel
// automatically becomes the channel's superadmin.
func (s *AccordServer) AddChannel(ctx context.Context, req *pb.AddChannelRequest) (*pb.AddChannelResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	username, err := getUsernameFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}
	if username == "" {
		return nil, status.Errorf(codes.InvalidArgument, "username cannot be empty")
	}

	ch := NewServerChannel(uint64(len(s.channels)), req.GetName(), req.GetIsPublic())
	ch.users[username] = &channelUser{
		user: s.authServer.users[username],
		role: SuperadminRole,
	}

	// TODO: add the new channel to the DB.
	// TODO: broadcast to ServerStream creation of new channel.
	s.channels[ch.channelId] = ch
	go ch.listen()

	res := &pb.AddChannelResponse{
		ChannelId: ch.channelId,
	}
	log.Printf("New Channel %s created", req.GetName())
	return res, nil
}

func (s *AccordServer) RemoveChannel(_ context.Context, req *pb.RemoveChannelRequest) (*pb.RemoveChannelResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	channelId := req.GetChannelId()
	if _, ok := s.channels[req.GetChannelId()]; ok {
		// TODO: remove the record from the DB.
		// TODO: broadcast to ServerStream removal of the channel.
		delete(s.channels, req.GetChannelId())
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "channel with Id %d doesn't exist", channelId)
	}

	res := &pb.RemoveChannelResponse{}
	log.Printf("Channel with id %d has been removed\n", channelId)
	return res, nil
}

func (s *AccordServer) GetChannels(ctx context.Context, req *pb.GetChannelsRequest) (*pb.GetChannelsResponse, error) {
	channel_metas := make(map[uint64]*pb.GetChannelsResponse_ChannelMeta)

	for k := range s.channels {
		meta := &pb.GetChannelsResponse_ChannelMeta{
			Name:         s.channels[k].name,
			IsPublic:     s.channels[k].isPublic,
			MembersCount: int32(len(s.channels[k].users)),
		}
		channel_metas[k] = meta
	}

	res := &pb.GetChannelsResponse{
		ChannelMetas: channel_metas,
	}

	return res, nil
}

func (s *AccordServer) GetChannel(ctx context.Context, req *pb.GetChannelRequest) (*pb.GetChannelResponse, error) {
	channel, ok := s.channels[req.GetChannelId()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "Channel with Id %d doesn't exist", req.GetChannelId())
	}

	users := make(map[string]*pb.GetChannelResponse_User)

	for uname, user := range channel.users {
		users[uname] = &pb.GetChannelResponse_User{
			Username: uname,
			Role:     int32(user.role),
		}
	}
	info := &pb.GetChannelResponse_ChannelInfo{
		ChannelId:   channel.channelId,
		Name:        channel.name,
		Users:       users,
		PinnedMsgId: channel.pinnedMsgId,
		IsPublic:    channel.isPublic,
	}

	res := &pb.GetChannelResponse{
		Channel: info,
	}
	return res, nil
}

// ChannelStream is the implementation of bidirectional streaming of client
// with one channel on the server.
func (s *AccordServer) ChannelStream(srv pb.Chat_ChannelStreamServer) error {
	var channel *ServerChannel = nil
	ctx := srv.Context()

	username, err := getUsernameFromContext(ctx)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to get username from context")
	}
	if username == "" {
		return status.Errorf(codes.InvalidArgument, "username cannot be empty")
	}

	for {
		req, err := srv.Recv()
		if err != nil {
			log.Fatalf("Error while reading client stream: %v", err)
		}

		if channel == nil {
			channel = s.channels[req.GetChannelId()]
			if channel == nil {
				return status.Errorf(codes.InvalidArgument, "invalid channel Id: %v", err)
			}
			// so far, authomatically add user as a member when he subscribes to the channel
			// TODO: add some RPC for user to request to join the channel with particular role.
			if _, ok := channel.users[username]; !ok {
				channel.users[username] = &channelUser{
					user: s.authServer.users[username],
					role: MemberRole,
				}
			}
			// add the stream for broadcasting to the user
			channel.usersToStreams[username] = srv
		} else if reqChannelId := req.GetChannelId(); channel.channelId != reqChannelId {
			return status.Errorf(codes.InvalidArgument, "each stream has to use consistent channel Ids\nhave:%d\nwant:%d\n", reqChannelId, channel.channelId)
		}

		select {
		// handle abrupt client disconnection
		case <-ctx.Done():
			channel.usersToStreams[username] = nil
			return status.Error(codes.Canceled, ctx.Err().Error())
		case channel.msgc <- req:
		}
	}

	return nil
}

func (s *AccordServer) Listen(serv_addr string) (string, error) {
	listener, err := net.Listen("tcp", serv_addr)
	if err != nil {
		log.Print("Failed to init listener:", err)
		return "", err
	}
	log.Print("Initialized listener:", listener.Addr().String())

	s.listener = listener
	return s.listener.Addr().String(), nil
}

func (s *AccordServer) Start() {
	tlsCredentials, err := loadTLSCredentials()
	if err != nil {
		log.Fatal("Cannot load TLS credentials:", err)
	}
	serverOptions := []grpc.ServerOption{
		grpc.UnaryInterceptor(s.authInterceptor.Unary()),
		grpc.StreamInterceptor(s.authInterceptor.Stream()),
	}

	serverOptions = append(serverOptions, grpc.Creds(tlsCredentials))

	srv := grpc.NewServer(serverOptions...)
	pb.RegisterAuthServiceServer(srv, s.authServer)
	pb.RegisterChatServer(srv, s)

	srv.Serve(s.listener)
}
