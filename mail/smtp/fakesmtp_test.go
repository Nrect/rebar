package smtp_test

import (
	"io"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeQueueID — id, который fakeServer называет в ответе на DATA.
const fakeQueueID = "Q-123"

// scenario — ответы fakeServer; задаётся до старта, иначе гонка с горутиной соединения.
type scenario struct {
	rcptReply     string // ответ на RCPT TO; пусто — 250
	dataReply     string // ответ на конец DATA; пусто — 250 с fakeQueueID
	rsetReply     string // ответ на RSET; пусто — 250
	advertiseAuth bool   // объявлять AUTH PLAIN LOGIN в EHLO
	hangOnData    bool   // молчать на DATA до закрытия соединения — тест ctx
}

// fakeServer — минимальный ESMTP без TLS: настоящий *gomail.SendError иначе
// не получить, его поля закрыты.
type fakeServer struct {
	addr string
	sc   scenario

	mu       sync.Mutex
	conns    []net.Conn
	commands []string
	message  string
}

func startFakeServer(t *testing.T, sc scenario) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &fakeServer{addr: ln.Addr().String(), sc: sc}
	go s.serve(ln)
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			_ = c.Close()
		}
	})
	return s
}

func (s *fakeServer) hostPort(t *testing.T) (host string, port int) {
	t.Helper()
	return splitHostPort(t, s.addr)
}

func splitHostPort(t *testing.T, addr string) (host string, port int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err = strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

func (s *fakeServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	reply := func(lines ...string) {
		for _, l := range lines {
			_ = tp.PrintfLine("%s", l)
		}
	}
	reply("220 fake ESMTP ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, _, _ := strings.Cut(strings.ToUpper(line), " ")
		s.record(verb)
		switch verb {
		case "EHLO", "HELO":
			ext := []string{"250-fake greets you", "250-ENHANCEDSTATUSCODES", "250-8BITMIME"}
			if s.sc.advertiseAuth {
				ext = append(ext, "250-AUTH PLAIN LOGIN")
			}
			reply(append(ext, "250 HELP")...)
		case "AUTH":
			reply("235 2.7.0 Authentication successful")
		case "NOOP", "MAIL":
			reply("250 2.0.0 Ok")
		case "RCPT":
			reply(orDefault(s.sc.rcptReply, "250 2.1.5 Ok"))
		case "DATA":
			if s.sc.hangOnData {
				_, _ = io.Copy(io.Discard, conn) // молчим, пока клиент не закроет
				return
			}
			reply("354 End data with <CR><LF>.<CR><LF>")
			body, readErr := io.ReadAll(tp.DotReader())
			if readErr != nil {
				return
			}
			s.setMessage(string(body))
			reply(orDefault(s.sc.dataReply, "250 2.0.0 Ok: queued as "+fakeQueueID))
		case "RSET":
			reply(orDefault(s.sc.rsetReply, "250 2.0.0 Ok"))
		case "QUIT":
			reply("221 2.0.0 Bye")
			return
		default:
			reply("502 5.5.1 Command not implemented")
		}
	}
}

func (s *fakeServer) record(verb string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, verb)
}

func (s *fakeServer) setMessage(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = m
}

// saw — видел ли сервер команду.
func (s *fakeServer) saw(verb string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.commands, verb)
}

// received — сырое письмо, принятое в DATA.
func (s *fakeServer) received() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.message
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
