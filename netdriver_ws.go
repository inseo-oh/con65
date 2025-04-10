// Copyright (c) 2025, Oh Inseo (YJK) -- Licensed under BSD-2-Clause
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

type wsClientConn struct {
	conn   *websocket.Conn
	closed bool
	msgBuf []uint8
}

var wsUpgrader = websocket.Upgrader{} // use default options
var wsPath = "/con65"

func startWsServer(serverAddr string) {
	http.Handle("/", http.FileServer(http.Dir("www")))
	http.HandleFunc(wsPath, serveWsClient)
	log.Printf("Started HTTP(WebSocket) server at %s%s", serverAddr, wsPath)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}

func serveWsClient(w http.ResponseWriter, r *http.Request) {
	log.Printf("New client connection from %s", r.RemoteAddr)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("websocket upgrade error:", err)
		return
	}
	defer conn.Close()

	clientConn := wsClientConn{
		conn: conn,
	}
	ctx := clientContext{
		logger: log.New(log.Writer(), fmt.Sprintf("[client/%s] ", conn.RemoteAddr()), log.Flags()),
		conn:   &clientConn,
	}
	for !clientConn.closed {
		err := ctx.serveNextCmd()
		if err != nil {
			ctx.logger.Printf("Closing client connection due to an error: %v", err)
			break
		}
	}
	ctx.logger.Printf("Closing client connection")
	conn.Close()
	ctx.logger.Printf("Closed client connection")
}

func (conn *wsClientConn) close() {
	conn.closed = true
}
func (conn *wsClientConn) out(b sendBuf) error {
	// Make sure we were not wasting more space by accident
	// TODO: Move this check to the main code.
	if len(b.dest) != 0 {
		panic("too many bytes were allocated")
	}
	return conn.conn.WriteMessage(websocket.BinaryMessage, b.buf)
}
func (conn *wsClientConn) recvMsg() error {
	tp, msg, err := conn.conn.ReadMessage()
	if err != nil {
		return err
	}
	if tp != websocket.BinaryMessage {
		return errors.New("expected binary message, got something else")
	}
	conn.msgBuf = append(conn.msgBuf, msg...)
	return nil
}
func (conn *wsClientConn) inB() (uint8, error) {
	if len(conn.msgBuf) < 1 {
		if err := conn.recvMsg(); err != nil {
			return 0, err
		}
	}
	res := conn.msgBuf[0]
	conn.msgBuf = conn.msgBuf[1:]
	return res, nil
}
func (conn *wsClientConn) inW() (uint16, error) {
	if len(conn.msgBuf) < 1 {
		if err := conn.recvMsg(); err != nil {
			return 0, err
		}
	}
	res := (uint16(conn.msgBuf[0]) << 8) | uint16(conn.msgBuf[1])
	conn.msgBuf = conn.msgBuf[2:]
	return res, nil
}
