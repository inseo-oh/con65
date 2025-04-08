package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
)

func main() {
	initDecoders()

	addr := "127.0.0.1:6502"
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen to connection -- %v", err)
	}
	log.Printf("Started server at %s", addr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept to connection -- %v", err)
			continue
		}
		log.Printf("New client connection from %s", conn.RemoteAddr().String())
		clientCtx := clientContext{
			conn:   conn,
			reader: bufio.NewReader(conn),
		}
		clientCtx.main()
		conn.Close()
	}
}

//==============================================================================
// State
//==============================================================================

type clientContext struct {
	conn   net.Conn
	reader *bufio.Reader
	closed bool

	ir uint8 // Instruction register

	// Registers ---------------------------------------------------------------
	regA  uint8  // Accumulator
	regX  uint8  // X register
	regY  uint8  // Y register
	regS  uint8  // Stack pointer
	regP  uint8  // Processor status
	regPC uint16 // Program counter

	//
	traceExec bool
}

//==============================================================================
// Memory bus
//==============================================================================

type busDir uint8

const (
	busDirRead = busDir(iota)
	busDirWrite
)

func (ctx *clientContext) readBus(addr uint16) (uint8, error) {
	return ctx.eventReadBus(addr)
}
func (ctx *clientContext) writeBus(addr uint16, v uint8) error {
	return ctx.eventWriteBus(addr, v)
}
func (ctx *clientContext) readMemW(addr uint16) (uint16, error) {
	res := uint16(0)
	if v, err := ctx.readBus(addr); err != nil {
		return 0, err
	} else {
		res = uint16(v)
	}
	if v, err := ctx.readBus(addr + 1); err != nil {
		return 0, err
	} else {
		res |= uint16(v) << 8
	}
	return res, nil
}
func (ctx *clientContext) readMemB(addr uint16) (uint8, error) {
	return ctx.readBus(addr)
}
func (ctx *clientContext) writeMemW(addr uint16, v uint16) error {
	if err := ctx.writeBus(addr, uint8(v)); err != nil {
		return err
	}
	if err := ctx.writeBus(addr+1, uint8(v>>8)); err != nil {
		return err
	}
	return nil
}
func (ctx *clientContext) writeMemB(addr uint16, v uint8) error {
	return ctx.writeBus(addr, v)

}

//==============================================================================
// Networking
//==============================================================================

// Every message(request or response) starts with header byte telling what kind of message it's sending
// Note that commands always come from the client
type netOpbyte uint8

const (
	// 0x - Response type.
	// Every response starts with this byte,
	netOpbyteAck  = netOpbyte(0x00) // Acknowledged
	netOpbyteFail = netOpbyte(0x01) // Failed

	// 1x - General commands
	netOpbyteBye          = netOpbyte(0x10) // Close the connection
	netOpbyteTraceExecOn  = netOpbyte(0x11) // Trace Execution - Enable
	netOpbyteTraceExecOff = netOpbyte(0x12) // Trace Execution - Disable
	netOpbyteTick         = netOpbyte(0x1f) // Run the CPU for a tick

	// 2x - CPU state manipulation commands
	netOpbyteWriteA  = netOpbyte(0x20) // Accumulator write
	netOpbyteReadA   = netOpbyte(0x21) // Accumulator read
	netOpbyteWriteX  = netOpbyte(0x22) // X Register write
	netOpbyteReadX   = netOpbyte(0x23) // X Register read
	netOpbyteWriteY  = netOpbyte(0x24) // Y Register write
	netOpbyteReadY   = netOpbyte(0x25) // Y Register read
	netOpbyteWriteS  = netOpbyte(0x26) // Stack pointer write
	netOpbyteReadS   = netOpbyte(0x27) // Stack pointer read
	netOpbyteWriteP  = netOpbyte(0x28) // PC write
	netOpbyteReadP   = netOpbyte(0x29) // PC read
	netOpbyteWritePc = netOpbyte(0x2a) // PC write
	netOpbyteReadPc  = netOpbyte(0x2b) // PC read

	// 8x - Server events
	// When client receives one of these, it should respond to it accordingly.
	netOpbyteEventReadBus   = netOpbyte(0x80) // Read from address
	netOpbyteEventWriteBus  = netOpbyte(0x81) // Write to address
	netOpbyteEventTraceExec = netOpbyte(0x82) // Event for Trace Execution
)

func (ctx *clientContext) main() {
	logger := log.New(log.Writer(), fmt.Sprintf("[client/%s] ", ctx.conn.RemoteAddr()), log.Flags())
	for !ctx.closed {
		err := ctx.serveNextCmd(logger)
		if err != nil {
			logger.Printf("Closing client connection due to an error: %v", err)
			break
		}
	}
	logger.Printf("Closing client connection")
	ctx.conn.Close()
	logger.Printf("Closed client connection")
}
func (ctx *clientContext) serveNextCmd(logger *log.Logger) error {
	const (
		debugNetmsg = false
	)

	var hdrByte uint8
	hdrByte, err := ctx.inB()
	if err != nil {
		return err
	}
	switch netOpbyte(hdrByte) {
	case netOpbyteBye:
		if debugNetmsg {
			logger.Printf("Bye")
		}
		ctx.closed = true

	case netOpbyteTraceExecOn:
		if debugNetmsg {
			logger.Printf("TraceExecOn")
		}
		ctx.traceExec = true
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteTraceExecOff:
		if debugNetmsg {
			logger.Printf("TraceExecOff")
		}
		ctx.traceExec = false
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteTick:
		if debugNetmsg {
			logger.Printf("Tick")
		}
		err := ctx.runNextInstr()
		if err != nil {
			res := newNetFailResponse()
			if err := ctx.out(res); err != nil {
				return err
			}
		}
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWriteA:
		val, err := ctx.inB()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WriteA %#x", val)
		}
		ctx.regA = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadA:
		if debugNetmsg {
			logger.Printf("ReadA")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.regA)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWriteX:
		val, err := ctx.inB()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WriteX %#x", val)
		}
		ctx.regX = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadX:
		if debugNetmsg {
			logger.Printf("ReadX")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.regX)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWriteY:
		val, err := ctx.inB()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WriteY %#x", val)
		}
		ctx.regY = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadY:
		if debugNetmsg {
			logger.Printf("ReadY")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.regY)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWriteS:
		val, err := ctx.inB()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WriteS %#x", val)
		}
		ctx.regS = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadS:
		if debugNetmsg {
			logger.Printf("ReadS")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.regS)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWriteP:
		val, err := ctx.inB()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WriteP %#x", val)
		}
		ctx.regP = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadP:
		if debugNetmsg {
			logger.Printf("ReadP")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.regP)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteWritePc:
		val, err := ctx.inW()
		if err != nil {
			return err
		}
		if debugNetmsg {
			logger.Printf("WritePc %#x", val)
		}
		ctx.regPC = val
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadPc:
		if debugNetmsg {
			logger.Printf("ReadPc")
		}
		res := newNetAckResponse(2)
		res.appendW(ctx.regPC)

		if err := ctx.out(res); err != nil {
			return err
		}

	default:
		logger.Printf("Unrecognized message type %x", hdrByte)
		if err := ctx.outFail(); err != nil {
			return err
		}
	}
	return nil
}

func (ctx *clientContext) eventReadBus(addr uint16) (uint8, error) {
	// Send event --------------------------------------------------------------
	event := newNetEvent(netOpbyteEventReadBus, 2)
	event.appendW(addr)
	if err := ctx.out(event); err != nil {
		return 0, err
	}
	// Receive response --------------------------------------------------------
	if err := ctx.expectAckOrFail(); err != nil {
		return 0, err
	}
	return ctx.inB()
}
func (ctx *clientContext) eventWriteBus(addr uint16, v uint8) error {
	// Send event --------------------------------------------------------------
	event := newNetEvent(netOpbyteEventWriteBus, 3)
	event.appendW(addr)
	event.appendB(v)
	if err := ctx.out(event); err != nil {
		return err
	}
	// Receive response --------------------------------------------------------
	return ctx.expectAckOrFail()
}
func (ctx *clientContext) eventTraceExec(pc uint16, ir uint8, disasm string) error {
	// Send event --------------------------------------------------------------
	event := newNetEvent(netOpbyteEventTraceExec, 4+len(disasm))
	event.appendW(pc)
	event.appendB(ir)
	event.appendS(disasm)
	if err := ctx.out(event); err != nil {
		return err
	}
	// Receive response --------------------------------------------------------
	return ctx.expectAckOrFail()
}

type sendBuf struct {
	buf  []uint8
	dest []uint8
}

func newNetEvent(typ netOpbyte, restLen int) sendBuf {
	buf := make([]uint8, restLen+1)
	buf[0] = uint8(typ)
	return sendBuf{buf: buf, dest: buf[1:]}
}
func newNetAckResponse(restLen int) sendBuf {
	buf := make([]uint8, restLen+1)
	buf[0] = uint8(netOpbyteAck)
	return sendBuf{buf: buf, dest: buf[1:]}
}
func newNetFailResponse() sendBuf {
	buf := make([]uint8, 1)
	buf[0] = uint8(netOpbyteFail)
	return sendBuf{buf: buf, dest: buf[1:]}
}

func (b *sendBuf) appendB(v uint8) {
	b.dest[0] = v
	b.dest = b.dest[1:]
}
func (b *sendBuf) appendW(v uint16) {
	binary.BigEndian.PutUint16(b.dest[0:2], v)
	b.dest = b.dest[2:]
}
func (b *sendBuf) appendS(s string) {
	if 255 < len(s) {
		panic("string cannot be sent because it's too long(max: 255 bytes)")
	}
	b.appendB(byte(len(s)))
	for i := range len(s) {
		b.dest[0] = s[i]
		b.dest = b.dest[1:]
	}
}

func (ctx *clientContext) out(b sendBuf) error {
	// Make sure we were not wasting more space by accident
	if len(b.dest) != 0 {
		panic("too many bytes were allocated")
	}
	_, err := ctx.conn.Write(b.buf)
	return err
}
func (ctx *clientContext) outFail() error {
	return ctx.out(newNetFailResponse())
}

func (ctx *clientContext) inB() (uint8, error) {
	return ctx.reader.ReadByte()
}
func (ctx *clientContext) inW() (uint16, error) {
	bytes := [2]uint8{}
	_, err := io.ReadFull(ctx.reader, bytes[:])
	if err != nil {
		return 0, err
	}
	res := (uint16(bytes[0]) << 8) | uint16(bytes[1])
	return res, nil
}
func (ctx *clientContext) expectAckOrFail() error {
	ackByte, err := ctx.inB()
	if err != nil {
		return err
	}
	switch netOpbyte(ackByte) {
	case netOpbyteAck:
		return nil
	case netOpbyteFail:
		return fmt.Errorf("communication error: expected ACK(%#x) got FAIL(%#x)", netOpbyteAck, netOpbyteFail)
	default:
		return fmt.Errorf("communication error: expected ACK(%#x) or FAIL(%#x), got %#x", netOpbyteAck, netOpbyteFail, ackByte)
	}
}

//==============================================================================
// Instruction declaration & decoding
//==============================================================================

type instr interface {
	disasm() string
	exec(ctx *clientContext) error
}
type decoderFunc func(ctx *clientContext) (instr, error)

var instrDecoders [256]decoderFunc

func initDecoders() {
	// NOP
	instrDecoders[0xea] = decodeEaNop
}

func (ctx *clientContext) fetchInstrB() (uint8, error) {
	res, err := ctx.readMemB(ctx.regPC)
	if err != nil {
		return 0, err
	}
	ctx.regPC += 1
	return res, nil
}
func (ctx *clientContext) fetchInstrW() (uint16, error) {
	res, err := ctx.readMemW(ctx.regPC)
	if err != nil {
		return 0, err
	}
	ctx.regPC += 2
	return res, nil
}
func (ctx *clientContext) dummyReadAtPc() {
	ctx.readMemB(ctx.regPC) // Dummy cycle
}

func (ctx *clientContext) runNextInstr() error {
	instrPc := ctx.regPC
	var decoder decoderFunc
	if v, err := ctx.fetchInstrB(); err != nil {
		return err
	} else {
		decoder = instrDecoders[v]
		if decoder == nil {
			log.Panicf("decoder for opcode %#x is not implemented", v)
		}
	}
	instr, err := decoder(ctx)
	if err != nil {
		return err
	}
	if ctx.traceExec {
		disasm := instr.disasm()
		if err := ctx.eventTraceExec(instrPc, ctx.ir, disasm); err != nil {
			return err
		}
	}
	return instr.exec(ctx)
}

// NOP -------------------------------------------------------------------------

type instrNop struct {
}

func (instr instrNop) disasm() string {
	return "nop"
}

func (instr instrNop) exec(ctx *clientContext) error {
	ctx.dummyReadAtPc()

	return nil
}

func decodeEaNop(ctx *clientContext) (instr, error) {
	return instrNop{}, nil
}
