package main

import (
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// delivery — что мини-сервер увидел за одно соединение.
type delivery struct {
	commands []string
	message  string
}

// scenario — ответы мини-сервера; задаётся до старта, иначе гонка с горутиной
// соединения.
type scenario struct {
	rcptReply string // ответ на RCPT TO; пусто — 250
}

// fakeSMTP — минимальный ESMTP без TLS и AUTH: ящик стенда глазами релея.
type fakeSMTP struct {
	addr       string
	sc         scenario
	deliveries chan delivery
}

func startFakeSMTP(t *testing.T, sc scenario) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &fakeSMTP{addr: ln.Addr().String(), sc: sc, deliveries: make(chan delivery, 8)}
	go s.serve(ln)
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTP) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	var d delivery
	defer func() { s.deliveries <- d }()

	_ = tp.PrintfLine("220 fake ESMTP ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, _, _ := strings.Cut(strings.ToUpper(line), " ")
		d.commands = append(d.commands, verb)
		switch verb {
		case "EHLO", "HELO":
			_ = tp.PrintfLine("250-fake greets you")
			_ = tp.PrintfLine("250 8BITMIME")
		case "MAIL", "RSET", "NOOP":
			_ = tp.PrintfLine("250 2.0.0 Ok")
		case "RCPT":
			_ = tp.PrintfLine("%s", orDefault(s.sc.rcptReply, "250 2.1.5 Ok"))
		case "DATA":
			_ = tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			raw, readErr := io.ReadAll(tp.DotReader())
			if readErr != nil {
				return
			}
			d.message = string(raw)
			_ = tp.PrintfLine("250 2.0.0 Ok: queued as FAKE-1")
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 Bye")
			return
		default:
			_ = tp.PrintfLine("502 5.5.1 Command not implemented")
		}
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
