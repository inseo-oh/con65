// Copyright (c) 2025, Oh Inseo (YJK) -- Licensed under BSD-2-Clause
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
)

type tcpClientConn struct {
	conn   net.Conn
	reader *bufio.Reader
	closed bool
}

func startTcpServer(serverAddr string) {
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		log.Fatalf("Failed to listen to connection -- %v", err)
	}
	log.Printf("Started TCP server at %s", serverAddr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept to connection -- %v", err)
			continue
		}
		log.Printf("New client connection from %s", conn.RemoteAddr().String())
		go serveTcpClient(conn)
	}
}

func serveTcpClient(conn net.Conn) {
	clientConn := tcpClientConn{
		conn:   conn,
		reader: bufio.NewReader(conn),
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

func (conn *tcpClientConn) close() {
	conn.closed = true
}
func (conn *tcpClientConn) out(b sendBuf) error {
	// Make sure we were not wasting more space by accident
	// TODO: Move this check to the main code.
	if len(b.dest) != 0 {
		panic("too many bytes were allocated")
	}
	_, err := conn.conn.Write(b.buf)
	return err
}

func (conn *tcpClientConn) inB() (uint8, error) {
	return conn.reader.ReadByte()
}
func (conn *tcpClientConn) inW() (uint16, error) {
	bytes := [2]uint8{}
	_, err := io.ReadFull(conn.reader, bytes[:])
	if err != nil {
		return 0, err
	}
	res := (uint16(bytes[0]) << 8) | uint16(bytes[1])
	return res, nil
}
