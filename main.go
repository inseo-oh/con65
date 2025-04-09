// Copyright (c) 2025, Oh Inseo (YJK) -- Licensed under BSD-2-Clause
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
	initInstrTable()

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
	regPC uint16 // Program counter

	// Processor flags (P register) --------------------------------------------
	flagN bool
	flagV bool
	flagD bool
	flagI bool
	flagZ bool
	flagC bool

	// Other flags -------------------------------------------------------------
	traceExec bool
}

const (
	RESET_VECTOR = uint16(0xfffc)
	BRK_VECTOR   = uint16(0xfffe)
)

//==============================================================================
// Processor status register
//==============================================================================

const (
	pFlagN = uint8(1 << 7)
	pFlagV = uint8(1 << 6)
	pFlagB = uint8(1 << 4) // Not an actual flag, but set by BRK and PHP
	pFlagD = uint8(1 << 3)
	pFlagI = uint8(1 << 2)
	pFlagZ = uint8(1 << 1)
	pFlagC = uint8(1 << 0)
)

func (ctx *clientContext) readP() uint8 {
	flags := uint8(0x20)
	if ctx.flagN {
		flags |= pFlagN
	}
	if ctx.flagV {
		flags |= pFlagV
	}
	if ctx.flagD {
		flags |= pFlagD
	}
	if ctx.flagI {
		flags |= pFlagI
	}
	if ctx.flagZ {
		flags |= pFlagZ
	}
	if ctx.flagC {
		flags |= pFlagC
	}
	return flags
}
func (ctx *clientContext) writeP(v uint8) {
	ctx.flagN = (v & pFlagN) != 0
	ctx.flagV = (v & pFlagV) != 0
	ctx.flagD = (v & pFlagD) != 0
	ctx.flagI = (v & pFlagI) != 0
	ctx.flagZ = (v & pFlagZ) != 0
	ctx.flagC = (v & pFlagC) != 0
}

//==============================================================================
// Stack
//==============================================================================

func (ctx *clientContext) push(v uint8) error {
	if err := ctx.writeMemB(0x100+uint16(ctx.regS), v); err != nil {
		return err
	}
	ctx.regS--
	return nil
}
func (ctx *clientContext) pushPc() error {
	if err := ctx.push(uint8(ctx.regPC >> 8)); err != nil {
		return err
	}
	if err := ctx.push(uint8(ctx.regPC)); err != nil {
		return err
	}
	return nil
}
func (ctx *clientContext) pull(isFirstPull bool) (uint8, error) {
	if isFirstPull {
		ctx.readMemB(0x100 + uint16(ctx.regS)) // Dummy read
	}
	ctx.regS++
	return ctx.readMemB(0x100 + uint16(ctx.regS))
}
func (ctx *clientContext) pullPc(isFirstPull bool) error {
	if v, err := ctx.pull(isFirstPull); err != nil {
		return err
	} else {
		ctx.regPC = uint16(v)
	}
	if v, err := ctx.pull(false); err != nil {
		return err
	} else {
		ctx.regPC |= uint16(v) << 8
	}
	return nil
}

//==============================================================================
// Memory bus
//==============================================================================

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
	netOpbyteEventReadBus       = netOpbyte(0x80) // Read from address
	netOpbyteEventWriteBus      = netOpbyte(0x81) // Write to address
	netOpbyteEventTraceExec     = netOpbyte(0x82) // Event for Trace Execution
	netOpbyteEventStop          = netOpbyte(0x83) // Event for Stop
	netOpbyteEventWaitInterrupt = netOpbyte(0x84) // Wait for interrupt
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
		ctx.writeP(val)
		res := newNetAckResponse(0)
		if err := ctx.out(res); err != nil {
			return err
		}

	case netOpbyteReadP:
		if debugNetmsg {
			logger.Printf("ReadP")
		}
		res := newNetAckResponse(1)
		res.appendB(ctx.readP())
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
func (ctx *clientContext) eventStop() error {
	// Send event --------------------------------------------------------------
	event := newNetEvent(netOpbyteEventStop, 0)
	if err := ctx.out(event); err != nil {
		return err
	}
	// Receive response --------------------------------------------------------
	return ctx.expectAckOrFail()
}
func (ctx *clientContext) eventWaitInterrupt() error {
	// Send event --------------------------------------------------------------
	event := newNetEvent(netOpbyteEventWaitInterrupt, 0)
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
// Instruction declaration, decoding, and execution
//==============================================================================

type instr struct {
	name     string
	execFn   func(ctx *clientContext, op operand) error
	addrmode addrmode
}

var instrs [256]*instr

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

func (ctx *clientContext) runNextInstr() error {
	instrPc := ctx.regPC
	// Fetch the opcode --------------------------------------------------------
	var instr *instr
	if v, err := ctx.fetchInstrB(); err != nil {
		return err
	} else {
		ctx.ir = v
		instr = instrs[v]
		if instr == nil {
			log.Panicf("opcode %#x is not implemented", v)
		}
	}
	// Decode the operand ------------------------------------------------------
	var operand operand
	operandDisasm := ""
	switch instr.addrmode {
	case addrmodeImp:
		ctx.readMemB(ctx.regPC) // Dummy read
		operand = impOperand{}
		operandDisasm = ""
	case addrmodeAcc:
		ctx.readMemB(ctx.regPC) // Dummy read
		operand = accOperand{}
		operandDisasm = "A"
	case addrmodeImm:
		val, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = immOperand{val}
		operandDisasm = fmt.Sprintf("#%#02x", val)
	case addrmodeAbs:
		// Check if the opcode is JSR
		addr := uint16(0)
		if ctx.ir == 0x20 {
			// JSR pushes PC after reading first byte of the absolute address, before 2nd byte is read.
			addrL, err := ctx.fetchInstrB()
			if err != nil {
				return err
			}
			ctx.readMemB(0x100 + uint16(ctx.regS)) // Dummy read
			if err := ctx.pushPc(); err != nil {
				return err
			}
			// Now we read remaining half of the operand
			addrH, err := ctx.fetchInstrB()
			if err != nil {
				return err
			}
			addr = (uint16(addrH) << 8) | uint16(addrL)
		} else if v, err := ctx.fetchInstrW(); err != nil {
			return err
		} else {
			addr = v
		}
		operand = absOperand{addr}
		operandDisasm = fmt.Sprintf("%#04x", addr)
	case addrmodeAbsX:
		addr, err := ctx.fetchInstrW()
		if err != nil {
			return err
		}
		operand = absXOperand{addr}
		operandDisasm = fmt.Sprintf("%#04x,x", addr)
	case addrmodeAbsY:
		addr, err := ctx.fetchInstrW()
		if err != nil {
			return err
		}
		operand = absYOperand{addr}
		operandDisasm = fmt.Sprintf("%#04x,y", addr)
	case addrmodeAbsInd:
		addr, err := ctx.fetchInstrW()
		if err != nil {
			return err
		}
		operand = absIndOperand{addr}
		operandDisasm = fmt.Sprintf("(%#04x)", addr)
	case addrmodeAbsIndX:
		addr, err := ctx.fetchInstrW()
		if err != nil {
			return err
		}
		operand = absIndXOperand{addr}
		operandDisasm = fmt.Sprintf("(%#04x,x)", addr)
	case addrmodeRel:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = relOperand{addr}
		operandDisasm = fmt.Sprintf("%#02x", addr)
	case addrmodeZp:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpOperand{addr}
		operandDisasm = fmt.Sprintf("%#02x", addr)
	case addrmodeZpX:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpXOperand{addr}
		operandDisasm = fmt.Sprintf("%#02x,x", addr)
	case addrmodeZpY:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpYOperand{addr}
		operandDisasm = fmt.Sprintf("%#02x,y", addr)
	case addrmodeZpInd:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpIndOperand{addr}
		operandDisasm = fmt.Sprintf("(%#02x)", addr)
	case addrmodeZpXInd:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpXIndOperand{addr}
		operandDisasm = fmt.Sprintf("(%#02x,x)", addr)
	case addrmodeZpIndY:
		addr, err := ctx.fetchInstrB()
		if err != nil {
			return err
		}
		operand = zpIndYOperand{addr}
		operandDisasm = fmt.Sprintf("(%#02x),y", addr)
	case addrmodeNop1b1c:
		operand = nopOperand{}
		operandDisasm = ""
	case addrmodeNop2b2c:
		ctx.fetchInstrB() // Ignored
		operand = nopOperand{}
		operandDisasm = ""
	default:
		panic("bad addrmode value")
	}
	// Emit trace execution event ----------------------------------------------
	if ctx.traceExec {
		disasm := fmt.Sprintf("%s %s", instr.name, operandDisasm)
		if err := ctx.eventTraceExec(instrPc, ctx.ir, disasm); err != nil {
			return err
		}
	}
	// Execute -----------------------------------------------------------------
	return instr.execFn(ctx, operand)
}

//==============================================================================
// Addressing modes
//==============================================================================

type addrmode uint8

const (
	addrmodeAcc     = addrmode(iota) // Accumulator
	addrmodeAbs                      // Absolute
	addrmodeAbsX                     // Absolute, X indexed
	addrmodeAbsY                     // Absolute, Y indexed
	addrmodeImm                      // Immediate
	addrmodeImp                      // Implied
	addrmodeAbsInd                   // (Absolute) Indirect
	addrmodeAbsIndX                  // (Absolute) Indirect, X indexed
	addrmodeZpInd                    // (Zeropage) Indirect
	addrmodeZpXInd                   // (Zeropage) X indexed indirect
	addrmodeZpIndY                   // (Zeropage) indirect Y indexed
	addrmodeRel                      // Relative
	addrmodeZp                       // Zeropage
	addrmodeZpX                      // Zeropage, X indexed
	addrmodeZpY                      // Zeropage, Y indexed
	// NOPs
	addrmodeNop1b1c // 1-byte, 1-cycle
	addrmodeNop2b2c // 2-byte, 2-cycle

)

type rmwDummyCycleType uint8

const (
	rmwDummyCycleTypeRead = rmwDummyCycleType(iota)
	rmwDummyCycleTypeWrite
)

type operand interface {
	read(ctx *clientContext) (uint8, error)
	write(ctx *clientContext, v uint8) error
	readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error
}

// Relative --------------------------------------------------------------------
type relOperand struct{ rel uint8 }

func (op relOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on an relative operand")
}
func (op relOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on an relative operand")
}
func (op relOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to read/write on an relative operand")
}

// Implied ---------------------------------------------------------------------
type impOperand struct{}

func (op impOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on an implied operand")
}
func (op impOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on an implied operand")
}
func (op impOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to read/write on an implied operand")
}

// Immediate ---------------------------------------------------------------------
type immOperand struct{ val uint8 }

func (op immOperand) read(ctx *clientContext) (uint8, error) {
	return op.val, nil
}
func (op immOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to write on an implied operand")
}
func (op immOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to write on an implied operand")
}

// Accumulator -----------------------------------------------------------------
type accOperand struct{}

func (op accOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.regA, nil
}
func (op accOperand) write(ctx *clientContext, v uint8) error {
	ctx.regA = v
	return nil
}
func (op accOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	ctx.regA = f(ctx.regA)
	return nil
}

// Absolute --------------------------------------------------------------------
type absOperand struct{ addr uint16 }

func (op absOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(op.addr)
}
func (op absOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(op.addr, v)
}
func (op absOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := op.addr
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Absolute, X indexed --------------------------------------------------------------------
type absXOperand struct{ addr uint16 }

func (op absXOperand) getAddr(ctx *clientContext, isWrite bool) uint16 {
	isInc := ctx.ir == 0xfe
	isDec := ctx.ir == 0xde
	isStz := ctx.ir == 0x9e
	addrH16 := ((op.addr & 0xff00) >> 8)
	addrL16 := (op.addr & 0xff) + uint16(ctx.regX)
	isPageCross := (addrL16 & 0xff00) != 0
	if isPageCross || isWrite {
		addrL16 &= 0xff
		// Certain instructions behaves a bit differently
		if isPageCross || (isInc || isDec || isStz) {
			ctx.readMemB(ctx.regPC - 1) // Dummy read
		}
		if isPageCross {
			addrH16++
		}
		if !isStz {
			ctx.readMemB(addrL16 | (addrH16 << 8)) // Dummy read
		}
	} else {
	}
	return addrL16 | (addrH16 << 8)
}
func (op absXOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(op.getAddr(ctx, false))
}
func (op absXOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(op.getAddr(ctx, true), v)
}
func (op absXOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := op.getAddr(ctx, true)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Absolute, Y indexed --------------------------------------------------------------------
type absYOperand struct{ addr uint16 }

func (op absYOperand) getAddr(ctx *clientContext, isWrite bool) uint16 {
	addrH16 := ((op.addr & 0xff00) >> 8)
	addrL16 := (op.addr & 0xff) + uint16(ctx.regY)
	isPageCross := (addrL16 & 0xff00) != 0
	if isPageCross || isWrite {
		addrL16 &= 0xff
		if isPageCross {
			ctx.readMemB(ctx.regPC - 1) // Dummy read
			addrH16++
		}
		ctx.readMemB(addrL16 | (addrH16 << 8)) // Dummy read
	} else {
	}
	return addrL16 | (addrH16 << 8)
}
func (op absYOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(op.getAddr(ctx, false))
}
func (op absYOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(op.getAddr(ctx, true), v)
}
func (op absYOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := op.getAddr(ctx, true)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Absolute indirect -----------------------------------------------------------
type absIndOperand struct{ addr uint16 }

// Only JMP uses this mode, and JMP handles it directly.
func (op absIndOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on an absolute indirect operand")
}
func (op absIndOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on an absolute indirect operand")
}
func (op absIndOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to read/write on an absolute indirect operand")
}

// Absolute X indexed indirect -------------------------------------------------
type absIndXOperand struct{ addr uint16 }

// Only JMP uses this mode, and JMP handles it directly.
func (op absIndXOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on an absolute indirect operand")
}
func (op absIndXOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on an absolute indirect operand")
}
func (op absIndXOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to read/write on an absolute indirect operand")
}

// Zeropage --------------------------------------------------------------------
type zpOperand struct{ addr uint8 }

func (op zpOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(uint16(op.addr))
}
func (op zpOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(uint16(op.addr), v)
}
func (op zpOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := uint16(op.addr)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Zeropage, X indexed ---------------------------------------------------------
type zpXOperand struct{ addr uint8 }

func (op zpXOperand) getAddr(ctx *clientContext) uint16 {
	addr8 := op.addr
	ctx.readMemB(uint16(addr8)) // Dummy read
	addr8 += ctx.regX
	return uint16(addr8)
}
func (op zpXOperand) read(ctx *clientContext) (uint8, error) {
	addr := op.getAddr(ctx)
	return ctx.readMemB(addr)
}
func (op zpXOperand) write(ctx *clientContext, v uint8) error {
	addr := op.getAddr(ctx)
	return ctx.writeMemB(addr, v)
}
func (op zpXOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := op.getAddr(ctx)

	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Zeropage, Y indexed ---------------------------------------------------------
type zpYOperand struct{ addr uint8 }

func (op zpYOperand) getAddr(ctx *clientContext) uint16 {
	addr8 := op.addr
	ctx.readMemB(uint16(addr8)) // Dummy read
	addr8 += ctx.regY
	return uint16(addr8)
}
func (op zpYOperand) read(ctx *clientContext) (uint8, error) {
	addr := op.getAddr(ctx)
	return ctx.readMemB(addr)
}
func (op zpYOperand) write(ctx *clientContext, v uint8) error {
	addr := op.getAddr(ctx)
	return ctx.writeMemB(addr, v)
}
func (op zpYOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr := op.getAddr(ctx)

	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Zeropage indirect -----------------------------------------------------------
type zpIndOperand struct{ addr uint8 }

func (op zpIndOperand) getAddr(ctx *clientContext) (uint16, error) {
	indAddr8 := op.addr
	realAddrL, err := ctx.readMemB(uint16(indAddr8))
	if err != nil {
		return 0, err
	}
	realAddrH, err := ctx.readMemB(uint16(indAddr8 + 1))
	if err != nil {
		return 0, err
	}
	realAddr := (uint16(realAddrH) << 8) | uint16(realAddrL)
	return realAddr, nil
}
func (op zpIndOperand) read(ctx *clientContext) (uint8, error) {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return 0, err
	}
	return ctx.readMemB(addr)
}
func (op zpIndOperand) write(ctx *clientContext, v uint8) error {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return err
	}
	return ctx.writeMemB(addr, v)
}
func (op zpIndOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return err
	}
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Zeropage, X indexed indirect ------------------------------------------------
type zpXIndOperand struct{ addr uint8 }

func (op zpXIndOperand) getAddr(ctx *clientContext) (uint16, error) {
	indAddr8 := op.addr
	ctx.readMemB(uint16(indAddr8)) // Dummy read
	indAddr8 += ctx.regX
	realAddrL, err := ctx.readMemB(uint16(indAddr8))
	if err != nil {
		return 0, err
	}
	realAddrH, err := ctx.readMemB(uint16(indAddr8 + 1))
	if err != nil {
		return 0, err
	}
	realAddr := (uint16(realAddrH) << 8) | uint16(realAddrL)
	return realAddr, nil
}
func (op zpXIndOperand) read(ctx *clientContext) (uint8, error) {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return 0, err
	}
	return ctx.readMemB(addr)
}
func (op zpXIndOperand) write(ctx *clientContext, v uint8) error {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return err
	}
	return ctx.writeMemB(addr, v)
}
func (op zpXIndOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return err
	}
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Zeropage, X indexed indirect ------------------------------------------------
type zpIndYOperand struct{ addr uint8 }

func (op zpIndYOperand) getAddr(ctx *clientContext, isWrite bool) (uint16, error) {
	indAddr8 := op.addr
	realAddrL, err := ctx.readMemB(uint16(indAddr8))
	if err != nil {
		return 0, err
	}
	realAddrH, err := ctx.readMemB(uint16(indAddr8 + 1))
	if err != nil {
		return 0, err
	}
	newAddrL16 := uint16(realAddrL) + uint16(ctx.regY)
	isPageCross := (newAddrL16 & 0xff00) != 0
	if isPageCross || isWrite {
		newAddrL16 &= 0xff
		ctx.readMemB(newAddrL16 | (uint16(realAddrH) << 8)) // Dummy read
		if isPageCross {
			realAddrH++
		}
	}
	realAddrL = uint8(newAddrL16)
	realAddr := (uint16(realAddrH) << 8) | uint16(realAddrL)

	return realAddr, nil
}
func (op zpIndYOperand) read(ctx *clientContext) (uint8, error) {
	addr, err := op.getAddr(ctx, false)
	if err != nil {
		return 0, err
	}
	return ctx.readMemB(addr)
}
func (op zpIndYOperand) write(ctx *clientContext, v uint8) error {
	addr, err := op.getAddr(ctx, true)
	if err != nil {
		return err
	}
	return ctx.writeMemB(addr, v)
}
func (op zpIndYOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	addr, err := op.getAddr(ctx, true)
	if err != nil {
		return err
	}
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	if dummyCycleType == rmwDummyCycleTypeRead {
		ctx.readMemB(addr) // Dummy read
	} else {
		ctx.writeMemB(addr, v) // Dummy write
	}
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// NOPs ------------------------------------------------------------------------

type nopOperand struct{}

func (op nopOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on NOP")
}
func (op nopOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on NOP")
}
func (op nopOperand) readModifyWrite(ctx *clientContext, dummyCycleType rmwDummyCycleType, f func(uint8) uint8) error {
	panic("attempted to read/write on an NOP")
}

//==============================================================================
// Instruction implementation
//==============================================================================

func initInstrTable() {
	// ASL ---------------------------------------------------------------------
	instrs[0x0a] = &instr{"asl", aslExec, addrmodeAcc}
	instrs[0x06] = &instr{"asl", aslExec, addrmodeZp}
	instrs[0x16] = &instr{"asl", aslExec, addrmodeZpX}
	instrs[0x0e] = &instr{"asl", aslExec, addrmodeAbs}
	instrs[0x1e] = &instr{"asl", aslExec, addrmodeAbsX}
	// LSR ---------------------------------------------------------------------
	instrs[0x4a] = &instr{"lsr", lsrExec, addrmodeAcc}
	instrs[0x46] = &instr{"lsr", lsrExec, addrmodeZp}
	instrs[0x56] = &instr{"lsr", lsrExec, addrmodeZpX}
	instrs[0x4e] = &instr{"lsr", lsrExec, addrmodeAbs}
	instrs[0x5e] = &instr{"lsr", lsrExec, addrmodeAbsX}
	// ROL ---------------------------------------------------------------------
	instrs[0x2a] = &instr{"rol", rolExec, addrmodeAcc}
	instrs[0x26] = &instr{"rol", rolExec, addrmodeZp}
	instrs[0x36] = &instr{"rol", rolExec, addrmodeZpX}
	instrs[0x2e] = &instr{"rol", rolExec, addrmodeAbs}
	instrs[0x3e] = &instr{"rol", rolExec, addrmodeAbsX}
	// ROR ---------------------------------------------------------------------
	instrs[0x6a] = &instr{"ror", rorExec, addrmodeAcc}
	instrs[0x66] = &instr{"ror", rorExec, addrmodeZp}
	instrs[0x76] = &instr{"ror", rorExec, addrmodeZpX}
	instrs[0x6e] = &instr{"ror", rorExec, addrmodeAbs}
	instrs[0x7e] = &instr{"ror", rorExec, addrmodeAbsX}
	// AND ---------------------------------------------------------------------
	instrs[0x29] = &instr{"and", andExec, addrmodeImm}
	instrs[0x25] = &instr{"and", andExec, addrmodeZp}
	instrs[0x35] = &instr{"and", andExec, addrmodeZpX}
	instrs[0x2d] = &instr{"and", andExec, addrmodeAbs}
	instrs[0x3d] = &instr{"and", andExec, addrmodeAbsX}
	instrs[0x39] = &instr{"and", andExec, addrmodeAbsY}
	instrs[0x21] = &instr{"and", andExec, addrmodeZpXInd}
	instrs[0x31] = &instr{"and", andExec, addrmodeZpIndY}
	instrs[0x32] = &instr{"and", andExec, addrmodeZpInd}
	// EOR ---------------------------------------------------------------------
	instrs[0x49] = &instr{"eor", eorExec, addrmodeImm}
	instrs[0x45] = &instr{"eor", eorExec, addrmodeZp}
	instrs[0x55] = &instr{"eor", eorExec, addrmodeZpX}
	instrs[0x4d] = &instr{"eor", eorExec, addrmodeAbs}
	instrs[0x5d] = &instr{"eor", eorExec, addrmodeAbsX}
	instrs[0x59] = &instr{"eor", eorExec, addrmodeAbsY}
	instrs[0x41] = &instr{"eor", eorExec, addrmodeZpXInd}
	instrs[0x51] = &instr{"eor", eorExec, addrmodeZpIndY}
	instrs[0x52] = &instr{"eor", eorExec, addrmodeZpInd}
	// ORA ---------------------------------------------------------------------
	instrs[0x09] = &instr{"ora", oraExec, addrmodeImm}
	instrs[0x05] = &instr{"ora", oraExec, addrmodeZp}
	instrs[0x15] = &instr{"ora", oraExec, addrmodeZpX}
	instrs[0x0d] = &instr{"ora", oraExec, addrmodeAbs}
	instrs[0x1d] = &instr{"ora", oraExec, addrmodeAbsX}
	instrs[0x19] = &instr{"ora", oraExec, addrmodeAbsY}
	instrs[0x01] = &instr{"ora", oraExec, addrmodeZpXInd}
	instrs[0x11] = &instr{"ora", oraExec, addrmodeZpIndY}
	instrs[0x12] = &instr{"ora", oraExec, addrmodeZpInd}
	// LDA ---------------------------------------------------------------------
	instrs[0xa9] = &instr{"lda", ldaExec, addrmodeImm}
	instrs[0xa5] = &instr{"lda", ldaExec, addrmodeZp}
	instrs[0xb5] = &instr{"lda", ldaExec, addrmodeZpX}
	instrs[0xad] = &instr{"lda", ldaExec, addrmodeAbs}
	instrs[0xbd] = &instr{"lda", ldaExec, addrmodeAbsX}
	instrs[0xb9] = &instr{"lda", ldaExec, addrmodeAbsY}
	instrs[0xa1] = &instr{"lda", ldaExec, addrmodeZpXInd}
	instrs[0xb1] = &instr{"lda", ldaExec, addrmodeZpIndY}
	instrs[0xb2] = &instr{"lda", ldaExec, addrmodeZpInd}
	// LDX ---------------------------------------------------------------------
	instrs[0xa2] = &instr{"ldx", ldxExec, addrmodeImm}
	instrs[0xa6] = &instr{"ldx", ldxExec, addrmodeZp}
	instrs[0xb6] = &instr{"ldx", ldxExec, addrmodeZpY}
	instrs[0xae] = &instr{"ldx", ldxExec, addrmodeAbs}
	instrs[0xbe] = &instr{"ldx", ldxExec, addrmodeAbsY}
	// LDY ---------------------------------------------------------------------
	instrs[0xa0] = &instr{"ldy", ldyExec, addrmodeImm}
	instrs[0xa4] = &instr{"ldy", ldyExec, addrmodeZp}
	instrs[0xb4] = &instr{"ldy", ldyExec, addrmodeZpX}
	instrs[0xac] = &instr{"ldy", ldyExec, addrmodeAbs}
	instrs[0xbc] = &instr{"ldy", ldyExec, addrmodeAbsX}
	// STA ---------------------------------------------------------------------
	instrs[0x85] = &instr{"sta", staExec, addrmodeZp}
	instrs[0x95] = &instr{"sta", staExec, addrmodeZpX}
	instrs[0x8d] = &instr{"sta", staExec, addrmodeAbs}
	instrs[0x9d] = &instr{"sta", staExec, addrmodeAbsX}
	instrs[0x99] = &instr{"sta", staExec, addrmodeAbsY}
	instrs[0x81] = &instr{"sta", staExec, addrmodeZpXInd}
	instrs[0x91] = &instr{"sta", staExec, addrmodeZpIndY}
	instrs[0x92] = &instr{"sta", staExec, addrmodeZpInd}
	// STX ---------------------------------------------------------------------
	instrs[0x86] = &instr{"stx", stxExec, addrmodeZp}
	instrs[0x96] = &instr{"stx", stxExec, addrmodeZpY}
	instrs[0x8e] = &instr{"stx", stxExec, addrmodeAbs}
	// STY ---------------------------------------------------------------------
	instrs[0x84] = &instr{"sty", styExec, addrmodeZp}
	instrs[0x94] = &instr{"sty", styExec, addrmodeZpX}
	instrs[0x8c] = &instr{"sty", styExec, addrmodeAbs}
	// STZ ---------------------------------------------------------------------
	instrs[0x64] = &instr{"stz", stzExec, addrmodeZp}
	instrs[0x74] = &instr{"stz", stzExec, addrmodeZpX}
	instrs[0x9c] = &instr{"stz", stzExec, addrmodeAbs}
	instrs[0x9e] = &instr{"stz", stzExec, addrmodeAbsX}
	// JMP ---------------------------------------------------------------------
	instrs[0x4c] = &instr{"jmp", jmpExec, addrmodeAbs}
	instrs[0x6c] = &instr{"jmp", jmpExec, addrmodeAbsInd}
	instrs[0x7c] = &instr{"jmp", jmpExec, addrmodeAbsIndX}
	// JSR ---------------------------------------------------------------------
	instrs[0x20] = &instr{"jsr", jsrExec, addrmodeAbs}
	// ADC ---------------------------------------------------------------------
	instrs[0x69] = &instr{"adc", adcExec, addrmodeImm}
	instrs[0x65] = &instr{"adc", adcExec, addrmodeZp}
	instrs[0x75] = &instr{"adc", adcExec, addrmodeZpX}
	instrs[0x6d] = &instr{"adc", adcExec, addrmodeAbs}
	instrs[0x7d] = &instr{"adc", adcExec, addrmodeAbsX}
	instrs[0x79] = &instr{"adc", adcExec, addrmodeAbsY}
	instrs[0x61] = &instr{"adc", adcExec, addrmodeZpXInd}
	instrs[0x71] = &instr{"adc", adcExec, addrmodeZpIndY}
	instrs[0x72] = &instr{"adc", adcExec, addrmodeZpInd}
	// SBC ---------------------------------------------------------------------
	instrs[0xe9] = &instr{"sbc", sbcExec, addrmodeImm}
	instrs[0xe5] = &instr{"sbc", sbcExec, addrmodeZp}
	instrs[0xf5] = &instr{"sbc", sbcExec, addrmodeZpX}
	instrs[0xed] = &instr{"sbc", sbcExec, addrmodeAbs}
	instrs[0xfd] = &instr{"sbc", sbcExec, addrmodeAbsX}
	instrs[0xf9] = &instr{"sbc", sbcExec, addrmodeAbsY}
	instrs[0xe1] = &instr{"sbc", sbcExec, addrmodeZpXInd}
	instrs[0xf1] = &instr{"sbc", sbcExec, addrmodeZpIndY}
	instrs[0xf2] = &instr{"sbc", sbcExec, addrmodeZpInd}
	// CMP ---------------------------------------------------------------------
	instrs[0xc9] = &instr{"cmp", cmpExec, addrmodeImm}
	instrs[0xc5] = &instr{"cmp", cmpExec, addrmodeZp}
	instrs[0xd5] = &instr{"cmp", cmpExec, addrmodeZpX}
	instrs[0xcd] = &instr{"cmp", cmpExec, addrmodeAbs}
	instrs[0xdd] = &instr{"cmp", cmpExec, addrmodeAbsX}
	instrs[0xd9] = &instr{"cmp", cmpExec, addrmodeAbsY}
	instrs[0xc1] = &instr{"cmp", cmpExec, addrmodeZpXInd}
	instrs[0xd1] = &instr{"cmp", cmpExec, addrmodeZpIndY}
	instrs[0xd2] = &instr{"cmp", cmpExec, addrmodeZpInd}
	// CPX ---------------------------------------------------------------------
	instrs[0xe0] = &instr{"cpx", cpxExec, addrmodeImm}
	instrs[0xe4] = &instr{"cpx", cpxExec, addrmodeZp}
	instrs[0xec] = &instr{"cpx", cpxExec, addrmodeAbs}
	// CPY---------------------------------------------------------------------
	instrs[0xc0] = &instr{"cpy", cpyExec, addrmodeImm}
	instrs[0xc4] = &instr{"cpy", cpyExec, addrmodeZp}
	instrs[0xcc] = &instr{"cpy", cpyExec, addrmodeAbs}
	// BIT ---------------------------------------------------------------------
	instrs[0x24] = &instr{"bit", bitExec, addrmodeZp}
	instrs[0x34] = &instr{"bit", bitExec, addrmodeZpX}
	instrs[0x2c] = &instr{"bit", bitExec, addrmodeAbs}
	instrs[0x3c] = &instr{"bit", bitExec, addrmodeAbsX}
	instrs[0x89] = &instr{"bit", bitExec, addrmodeImm}
	// TRB---------------------------------------------------------------------
	instrs[0x1c] = &instr{"trb", trbExec, addrmodeAbs}
	instrs[0x14] = &instr{"trb", trbExec, addrmodeZp}
	// TSB---------------------------------------------------------------------
	instrs[0x0c] = &instr{"tsb", tsbExec, addrmodeAbs}
	instrs[0x04] = &instr{"tsb", tsbExec, addrmodeZp}
	// INC ---------------------------------------------------------------------
	instrs[0x1a] = &instr{"inc", incExec, addrmodeAcc}
	instrs[0xe6] = &instr{"inc", incExec, addrmodeZp}
	instrs[0xf6] = &instr{"inc", incExec, addrmodeZpX}
	instrs[0xee] = &instr{"inc", incExec, addrmodeAbs}
	instrs[0xfe] = &instr{"inc", incExec, addrmodeAbsX}
	// DEC ---------------------------------------------------------------------
	instrs[0x3a] = &instr{"dec", decExec, addrmodeAcc}
	instrs[0xc6] = &instr{"dec", decExec, addrmodeZp}
	instrs[0xd6] = &instr{"dec", decExec, addrmodeZpX}
	instrs[0xce] = &instr{"dec", decExec, addrmodeAbs}
	instrs[0xde] = &instr{"dec", decExec, addrmodeAbsX}
	// RMB ---------------------------------------------------------------------
	instrs[0x07] = &instr{"rmb0", rmb0Exec, addrmodeZp}
	instrs[0x17] = &instr{"rmb1", rmb1Exec, addrmodeZp}
	instrs[0x27] = &instr{"rmb2", rmb2Exec, addrmodeZp}
	instrs[0x37] = &instr{"rmb3", rmb3Exec, addrmodeZp}
	instrs[0x47] = &instr{"rmb4", rmb4Exec, addrmodeZp}
	instrs[0x57] = &instr{"rmb5", rmb5Exec, addrmodeZp}
	instrs[0x67] = &instr{"rmb6", rmb6Exec, addrmodeZp}
	instrs[0x77] = &instr{"rmb7", rmb7Exec, addrmodeZp}
	// SMB ---------------------------------------------------------------------
	instrs[0x87] = &instr{"smb0", smb0Exec, addrmodeZp}
	instrs[0x97] = &instr{"smb1", smb1Exec, addrmodeZp}
	instrs[0xa7] = &instr{"smb2", smb2Exec, addrmodeZp}
	instrs[0xb7] = &instr{"smb3", smb3Exec, addrmodeZp}
	instrs[0xc7] = &instr{"smb4", smb4Exec, addrmodeZp}
	instrs[0xd7] = &instr{"smb5", smb5Exec, addrmodeZp}
	instrs[0xe7] = &instr{"smb6", smb6Exec, addrmodeZp}
	instrs[0xf7] = &instr{"smb7", smb7Exec, addrmodeZp}
	// Branch instructions -----------------------------------------------------
	instrs[0xb0] = &instr{"bcs", bcsExec, addrmodeRel}
	instrs[0x90] = &instr{"bcc", bccExec, addrmodeRel}
	instrs[0xf0] = &instr{"beq", beqExec, addrmodeRel}
	instrs[0xd0] = &instr{"bne", bneExec, addrmodeRel}
	instrs[0x10] = &instr{"bpl", bplExec, addrmodeRel}
	instrs[0x30] = &instr{"bmi", bmiExec, addrmodeRel}
	instrs[0x70] = &instr{"bvs", bvsExec, addrmodeRel}
	instrs[0x50] = &instr{"bvc", bvcExec, addrmodeRel}
	instrs[0x0f] = &instr{"bbr0", bbr0Exec, addrmodeRel}
	instrs[0x1f] = &instr{"bbr1", bbr1Exec, addrmodeRel}
	instrs[0x2f] = &instr{"bbr2", bbr2Exec, addrmodeRel}
	instrs[0x3f] = &instr{"bbr3", bbr3Exec, addrmodeRel}
	instrs[0x4f] = &instr{"bbr4", bbr4Exec, addrmodeRel}
	instrs[0x5f] = &instr{"bbr5", bbr5Exec, addrmodeRel}
	instrs[0x6f] = &instr{"bbr6", bbr6Exec, addrmodeRel}
	instrs[0x7f] = &instr{"bbr7", bbr7Exec, addrmodeRel}
	instrs[0x8f] = &instr{"bbs0", bbs0Exec, addrmodeRel}
	instrs[0x9f] = &instr{"bbs1", bbs1Exec, addrmodeRel}
	instrs[0xaf] = &instr{"bbs2", bbs2Exec, addrmodeRel}
	instrs[0xbf] = &instr{"bbs3", bbs3Exec, addrmodeRel}
	instrs[0xcf] = &instr{"bbs4", bbs4Exec, addrmodeRel}
	instrs[0xdf] = &instr{"bbs5", bbs5Exec, addrmodeRel}
	instrs[0xef] = &instr{"bbs6", bbs6Exec, addrmodeRel}
	instrs[0xff] = &instr{"bbs7", bbs7Exec, addrmodeRel}
	instrs[0x80] = &instr{"bra", braExec, addrmodeRel}
	// Instructions with implied operands --------------------------------------
	instrs[0x18] = &instr{"clc", clcExec, addrmodeImp}
	instrs[0xd8] = &instr{"cld", cldExec, addrmodeImp}
	instrs[0x58] = &instr{"cli", cliExec, addrmodeImp}
	instrs[0xb8] = &instr{"clv", clvExec, addrmodeImp}
	instrs[0x38] = &instr{"sec", secExec, addrmodeImp}
	instrs[0xf8] = &instr{"sed", sedExec, addrmodeImp}
	instrs[0x78] = &instr{"sei", seiExec, addrmodeImp}
	instrs[0xca] = &instr{"dex", dexExec, addrmodeImp}
	instrs[0x88] = &instr{"dey", deyExec, addrmodeImp}
	instrs[0xe8] = &instr{"inx", inxExec, addrmodeImp}
	instrs[0xc8] = &instr{"iny", inyExec, addrmodeImp}
	instrs[0x48] = &instr{"pha", phaExec, addrmodeImp}
	instrs[0x08] = &instr{"php", phpExec, addrmodeImp}
	instrs[0xda] = &instr{"phx", phxExec, addrmodeImp}
	instrs[0x5a] = &instr{"phy", phyExec, addrmodeImp}
	instrs[0x68] = &instr{"pla", plaExec, addrmodeImp}
	instrs[0x28] = &instr{"plp", plpExec, addrmodeImp}
	instrs[0xfa] = &instr{"plx", plxExec, addrmodeImp}
	instrs[0x7a] = &instr{"ply", plyExec, addrmodeImp}
	instrs[0x40] = &instr{"rti", rtiExec, addrmodeImp}
	instrs[0x60] = &instr{"rts", rtsExec, addrmodeImp}
	instrs[0xaa] = &instr{"tax", taxExec, addrmodeImp}
	instrs[0xa8] = &instr{"tax", tayExec, addrmodeImp}
	instrs[0xba] = &instr{"tax", tsxExec, addrmodeImp}
	instrs[0x8a] = &instr{"tax", txaExec, addrmodeImp}
	instrs[0x9a] = &instr{"tax", txsExec, addrmodeImp}
	instrs[0x98] = &instr{"tax", tyaExec, addrmodeImp}
	instrs[0x00] = &instr{"brk", brkExec, addrmodeImp}
	instrs[0xdb] = &instr{"stp", stpExec, addrmodeImp}
	instrs[0xcb] = &instr{"wai", waiExec, addrmodeImp}
	// NOPs --------------------------------------------------------------------
	instrs[0xea] = &instr{"nop", nopType1Exec, addrmodeImp}
	// 1 byte, 1 cycle
	{
		ops := [...]uint8{
			0x03, 0x13, 0x23, 0x33, 0x43, 0x53, 0x63, 0x73,
			0x83, 0x93, 0xa3, 0xb3, 0xc3, 0xd3, 0xe3, 0xf3,
			0x0b, 0x1b, 0x2b, 0x3b, 0x4b, 0x5b, 0x6b, 0x7b,
			0x8b, 0x9b, 0xab, 0xbb, 0xeb, 0xfb,
		}
		for _, op := range ops {
			instrs[op] = &instr{"nop", nopType1Exec, addrmodeNop1b1c}
		}
	}
	// 2 bytes, 2 cycles
	{
		opcodes := [...]uint8{
			0x02, 0x22, 0x42, 0x62, 0x82, 0xc2, 0xe2,
		}
		for _, op := range opcodes {
			instrs[op] = &instr{"nop", nopType1Exec, addrmodeNop2b2c}
		}
	}
	// 2 bytes, 3 cycles
	{
		opcodes := [...]uint8{
			0x44,
		}
		for _, op := range opcodes {
			instrs[op] = &instr{"nop", nopType2Exec, addrmodeZp}
		}
	}
	// 2 bytes, 4 cycles
	{
		opcodes := [...]uint8{
			0x54, 0xd4, 0xf4,
		}
		for _, op := range opcodes {
			instrs[op] = &instr{"nop", nopType2Exec, addrmodeZpX}
		}
	}
	// 3 bytes, 4 cycles
	{
		opcodes := [...]uint8{
			0xdc, 0xfc, 0x5c,
		}
		for _, op := range opcodes {
			instrs[op] = &instr{"nop", nopType3Exec, addrmodeAbs}
		}
	}
}

func (ctx *clientContext) setNZ(v uint8) {
	ctx.flagN = (v >> 7) != 0
	ctx.flagZ = v == 0x00
}

// Branch operations -----------------------------------------------------------
func signExtBtoW(v uint8) uint16 {
	return uint16(int16(int8(v)))
}

func doBranch(ctx *clientContext, op operand, cond bool) {
	rel, ok := op.(relOperand)
	if !ok {
		panic("expected an relative operand")
	}
	if !cond {
		return
	}
	ctx.readMemB(ctx.regPC) // Dummy read
	oldPcH := uint8(ctx.regPC >> 8)
	ctx.regPC += signExtBtoW(rel.rel)
	newPcH := uint8(ctx.regPC >> 8)
	isPageCross := oldPcH != newPcH
	if isPageCross {
		ctx.readMemB((ctx.regPC & 0xff) | uint16(oldPcH)<<8) // Dummy read
	}
}
func bcsExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, ctx.flagC)
	return nil
}
func bccExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, !ctx.flagC)
	return nil
}
func beqExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, ctx.flagZ)
	return nil
}
func bneExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, !ctx.flagZ)
	return nil
}
func bmiExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, ctx.flagN)
	return nil
}
func bplExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, !ctx.flagN)
	return nil
}
func bvsExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, ctx.flagV)
	return nil
}
func bvcExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, !ctx.flagV)
	return nil
}

// FIXME: BBRx and BBSx instructions are implemented, but seems to be broken.
func bbr0Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<0)) == 0)
	return nil
}
func bbr1Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<1)) == 0)
	return nil
}
func bbr2Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<2)) == 0)
	return nil
}
func bbr3Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<3)) == 0)
	return nil
}
func bbr4Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<4)) == 0)
	return nil
}
func bbr5Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<5)) == 0)
	return nil
}
func bbr6Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<6)) == 0)
	return nil
}
func bbr7Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<7)) == 0)
	return nil
}
func bbs0Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<0)) != 0)
	return nil
}
func bbs1Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<1)) != 0)
	return nil
}
func bbs2Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<2)) != 0)
	return nil
}
func bbs3Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<3)) != 0)
	return nil
}
func bbs4Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<4)) != 0)
	return nil
}
func bbs5Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<5)) != 0)
	return nil
}
func bbs6Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<6)) != 0)
	return nil
}
func bbs7Exec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, (ctx.regA&(1<<7)) != 0)
	return nil
}
func braExec(ctx *clientContext, op operand) error {
	doBranch(ctx, op, true)
	return nil
}

// ASL -------------------------------------------------------------------------
func aslExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		res := old << 1
		oldBit7 := (old & 0x80) != 0
		ctx.setNZ(res)
		ctx.flagC = oldBit7
		return res
	})
}

// LSR -------------------------------------------------------------------------
func lsrExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		res := old >> 1
		oldBit0 := (old & 0x1) != 0
		ctx.setNZ(res)
		ctx.flagC = oldBit0
		return res
	})
}

// ROL -------------------------------------------------------------------------
func rolExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		bit0 := ctx.flagC
		oldBit7 := (old & 0x80) != 0
		res := old << 1
		if bit0 {
			res |= 0x01
		}
		ctx.setNZ(res)
		ctx.flagC = oldBit7
		return res
	})
}

// ROR -------------------------------------------------------------------------
func rorExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		bit7 := ctx.flagC
		oldBit0 := (old & 0x01) != 0
		res := old >> 1
		if bit7 {
			res |= 0x80
		}
		ctx.setNZ(res)
		ctx.flagC = oldBit0
		return res
	})
}

// AND -------------------------------------------------------------------------
func andExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regA &= rhs
	ctx.setNZ(ctx.regA)
	return nil
}

// EOR -------------------------------------------------------------------------
func eorExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regA ^= rhs
	ctx.setNZ(ctx.regA)
	return nil
}

// ORA -------------------------------------------------------------------------
func oraExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regA |= rhs
	ctx.setNZ(ctx.regA)
	return nil
}

// LDA -------------------------------------------------------------------------
func ldaExec(ctx *clientContext, op operand) error {
	v, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regA = v
	ctx.setNZ(ctx.regA)
	return nil
}

// LDX -------------------------------------------------------------------------
func ldxExec(ctx *clientContext, op operand) error {
	v, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regX = v
	ctx.setNZ(ctx.regX)
	return nil
}

// LDY -------------------------------------------------------------------------
func ldyExec(ctx *clientContext, op operand) error {
	v, err := op.read(ctx)
	if err != nil {
		return err
	}
	ctx.regY = v
	ctx.setNZ(ctx.regY)
	return nil
}

// STA -------------------------------------------------------------------------
func staExec(ctx *clientContext, op operand) error {
	return op.write(ctx, ctx.regA)
}

// STX -------------------------------------------------------------------------
func stxExec(ctx *clientContext, op operand) error {
	return op.write(ctx, ctx.regX)
}

// STY -------------------------------------------------------------------------
func styExec(ctx *clientContext, op operand) error {
	return op.write(ctx, ctx.regY)
}

// STZ -------------------------------------------------------------------------
func stzExec(ctx *clientContext, op operand) error {
	return op.write(ctx, 0)
}

// JMP -------------------------------------------------------------------------
func jmpExec(ctx *clientContext, op operand) error {
	if absOp, ok := op.(absOperand); ok {
		ctx.regPC = absOp.addr
	} else if indOp, ok := op.(absIndOperand); ok {
		// XXX: This page says on 65C02, it will spend extra cycle when page crossing occurs while reading from indirect address:
		// https://www.masswerk.at/6502/6502_instruction_set.html
		// "there's an extra cycle added to the exuction time when address bytes are on different memory pages."
		//
		// But the JSON tests I'm using doesn't seem to agree with this, because as you can see below, there's no conditional dummy read.
		newPcL, err := ctx.readMemB(indOp.addr)
		if err != nil {
			return err
		}
		ctx.readMemB((indOp.addr & 0xff00) | ((indOp.addr + 1) & 0xff)) // Dummy read
		newPcH, err := ctx.readMemB(indOp.addr + 1)
		if err != nil {
			return err
		}
		ctx.regPC = uint16(newPcL) | (uint16(newPcH) << 8)
	} else if indOp, ok := op.(absIndXOperand); ok {
		indAddr := indOp.addr + uint16(ctx.regX)
		ctx.readMemB(ctx.regPC - 2) // Dummy read

		newPcL, err := ctx.readMemB(indAddr)
		if err != nil {
			return err
		}
		newPcH, err := ctx.readMemB(indAddr + 1)
		if err != nil {
			return err
		}
		ctx.regPC = uint16(newPcL) | (uint16(newPcH) << 8)
	} else {
		panic("bad addressing mode")
	}
	return nil
}

// JSR -------------------------------------------------------------------------
func jsrExec(ctx *clientContext, op operand) error {
	// Note that PC was already pushed before we reached here (due to unusual way JSR works)
	if absOp, ok := op.(absOperand); ok {
		ctx.regPC = absOp.addr
	} else {
		panic("bad addressing mode")
	}
	return nil
}

// ADC, SBC, CMP ---------------------------------------------------------------
func adcImpl(lhs, rhs uint8, carryIn bool) (res uint8, carryOut bool, overflow bool) {
	carryVal := uint8(0)
	if carryIn {
		carryVal = 1
	}
	result16 := uint16(lhs) + uint16(rhs) + uint16(carryVal)
	carryOut = (result16 & 0xff00) != 0
	res = uint8(result16)
	overflow =
		((lhs^rhs)&0x80 == 0) && // It's overflow if LHS and RHS signs are the same
			((lhs^res)&0x80 != 0) // and resulting sign is different
	return
}
func sbcImpl(lhs, rhs uint8, carryIn bool) (res uint8, carryOut bool, overflow bool) {
	return adcImpl(lhs, ^rhs, carryIn)
}
func (ctx *clientContext) cmpImpl(op operand, lhs uint8) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	res, carry, _ := sbcImpl(lhs, rhs, true)
	ctx.setNZ(res)
	ctx.flagC = carry
	return nil
}
func adcExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	res, carry, overflow := adcImpl(ctx.regA, rhs, ctx.flagC)
	ctx.regA = res
	ctx.flagC = carry
	ctx.flagV = overflow
	ctx.setNZ(res)
	return nil
}
func sbcExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	res, carry, overflow := sbcImpl(ctx.regA, rhs, ctx.flagC)
	ctx.regA = res
	ctx.flagC = carry
	ctx.flagV = overflow
	ctx.setNZ(res)
	return nil
}
func cmpExec(ctx *clientContext, op operand) error {
	return ctx.cmpImpl(op, ctx.regA)
}
func cpxExec(ctx *clientContext, op operand) error {
	return ctx.cmpImpl(op, ctx.regX)
}
func cpyExec(ctx *clientContext, op operand) error {
	return ctx.cmpImpl(op, ctx.regY)
}

// BIT -------------------------------------------------------------------------
func bitExec(ctx *clientContext, op operand) error {
	rhs, err := op.read(ctx)
	if err != nil {
		return err
	}
	res := ctx.regA & rhs
	ctx.setNZ(res)
	ctx.flagN = (rhs & 0x80) != 0
	ctx.flagV = (rhs & 0x40) != 0
	return nil
}

// TRB -------------------------------------------------------------------------
func trbExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		res := ctx.regA & old
		ctx.flagZ = res == 0x00
		return old & ^ctx.regA
	})
}

// TSB -------------------------------------------------------------------------
func tsbExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		res := ctx.regA & old
		ctx.flagZ = res == 0x00
		return old | ctx.regA
	})
}

// INC -------------------------------------------------------------------------
func incExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		v := old + 1
		ctx.setNZ(v)
		return v
	})
}

// DEC -------------------------------------------------------------------------
func decExec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		v := old - 1
		ctx.setNZ(v)
		return v
	})
}

// RMB -------------------------------------------------------------------------
func rmb0Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<0)
	})
}
func rmb1Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<1)
	})
}
func rmb2Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<2)
	})
}
func rmb3Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<3)
	})
}
func rmb4Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<4)
	})
}
func rmb5Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<5)
	})
}
func rmb6Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<6)
	})
}
func rmb7Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old & ^uint8(1<<7)
	})
}

// SMB -------------------------------------------------------------------------
func smb0Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<0)
	})
}
func smb1Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<1)
	})
}
func smb2Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<2)
	})
}
func smb3Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<3)
	})
}
func smb4Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<4)
	})
}
func smb5Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<5)
	})
}
func smb6Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<6)
	})
}
func smb7Exec(ctx *clientContext, op operand) error {
	return op.readModifyWrite(ctx, rmwDummyCycleTypeRead, func(old uint8) uint8 {
		return old | uint8(1<<7)
	})
}

// NOPs ------------------------------------------------------------------------
func nopType1Exec(ctx *clientContext, op operand) error {
	return nil
}
func nopType2Exec(ctx *clientContext, op operand) error {
	op.read(ctx) // Dummy read
	return nil
}
func nopType3Exec(ctx *clientContext, op operand) error {
	ctx.readMemB(ctx.regPC - 1) // Dummy read
	return nil
}

// Instructions with implied operands ------------------------------------------
func clcExec(ctx *clientContext, op operand) error {
	ctx.flagC = false
	return nil
}
func cldExec(ctx *clientContext, op operand) error {
	ctx.flagD = false
	return nil
}
func cliExec(ctx *clientContext, op operand) error {
	ctx.flagI = false
	return nil
}
func clvExec(ctx *clientContext, op operand) error {
	ctx.flagV = false
	return nil
}
func secExec(ctx *clientContext, op operand) error {
	ctx.flagC = true
	return nil
}
func sedExec(ctx *clientContext, op operand) error {
	ctx.flagD = true
	return nil
}
func seiExec(ctx *clientContext, op operand) error {
	ctx.flagI = true
	return nil
}
func dexExec(ctx *clientContext, op operand) error {
	ctx.regX--
	ctx.setNZ(ctx.regX)
	return nil
}
func deyExec(ctx *clientContext, op operand) error {
	ctx.regY--
	ctx.setNZ(ctx.regY)
	return nil
}
func inxExec(ctx *clientContext, op operand) error {
	ctx.regX++
	ctx.setNZ(ctx.regX)
	return nil
}
func inyExec(ctx *clientContext, op operand) error {
	ctx.regY++
	ctx.setNZ(ctx.regY)
	return nil
}
func phaExec(ctx *clientContext, op operand) error {
	return ctx.push(ctx.regA)
}
func phpExec(ctx *clientContext, op operand) error {
	return ctx.push(ctx.readP() | (1 << 4))
}
func phxExec(ctx *clientContext, op operand) error {
	return ctx.push(ctx.regX)
}
func phyExec(ctx *clientContext, op operand) error {
	return ctx.push(ctx.regY)
}
func plaExec(ctx *clientContext, op operand) error {
	v, err := ctx.pull(true)
	if err != nil {
		return err
	}
	ctx.regA = v
	ctx.setNZ(ctx.regA)
	return nil
}
func plpExec(ctx *clientContext, op operand) error {
	v, err := ctx.pull(true)
	if err != nil {
		return err
	}
	ctx.writeP(v)
	return nil
}
func plxExec(ctx *clientContext, op operand) error {
	v, err := ctx.pull(true)
	if err != nil {
		return err
	}
	ctx.regX = v
	ctx.setNZ(ctx.regX)
	return nil
}
func plyExec(ctx *clientContext, op operand) error {
	v, err := ctx.pull(true)
	if err != nil {
		return err
	}
	ctx.regY = v
	ctx.setNZ(ctx.regY)
	return nil
}
func rtiExec(ctx *clientContext, op operand) error {
	v, err := ctx.pull(true)
	if err != nil {
		return err
	}
	ctx.writeP(v)
	if err := ctx.pullPc(false); err != nil {
		return err
	}
	return nil
}
func rtsExec(ctx *clientContext, op operand) error {
	if err := ctx.pullPc(true); err != nil {
		return err
	}
	ctx.readMemB(ctx.regPC) // Dummy read
	// JSR pushes (return address - 1), so we have to add 1 back.
	ctx.regPC += 1

	return nil
}
func taxExec(ctx *clientContext, op operand) error {
	ctx.regX = ctx.regA
	ctx.setNZ(ctx.regX)
	return nil
}
func tayExec(ctx *clientContext, op operand) error {
	ctx.regY = ctx.regA
	ctx.setNZ(ctx.regY)
	return nil
}
func tsxExec(ctx *clientContext, op operand) error {
	ctx.regX = ctx.regS
	ctx.setNZ(ctx.regX)
	return nil
}
func txaExec(ctx *clientContext, op operand) error {
	ctx.regA = ctx.regX
	ctx.setNZ(ctx.regA)
	return nil
}
func txsExec(ctx *clientContext, op operand) error {
	ctx.regS = ctx.regX
	return nil
}
func tyaExec(ctx *clientContext, op operand) error {
	ctx.regA = ctx.regY
	ctx.setNZ(ctx.regA)
	return nil
}
func brkExec(ctx *clientContext, op operand) error {
	ctx.regPC++
	if err := ctx.pushPc(); err != nil {
		return err
	}
	if err := ctx.push(ctx.readP() | (1 << 4)); err != nil {
		return err
	}
	ctx.flagI = true
	if v, err := ctx.readMemW(BRK_VECTOR); err != nil {
		return err
	} else {
		ctx.regPC = v
	}
	return nil
}
func stpExec(ctx *clientContext, op operand) error {
	// STUB
	return ctx.eventStop()
}
func waiExec(ctx *clientContext, op operand) error {
	// STUB
	return ctx.eventWaitInterrupt()
}
