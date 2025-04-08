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
	instrOpcode := uint8(0)
	if v, err := ctx.fetchInstrB(); err != nil {
		return err
	} else {
		instrOpcode = v
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
		if instrOpcode == 0x20 {
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
		operandDisasm = fmt.Sprintf("%#04x", addr)
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
	addrmodeAcc    = addrmode(iota) // Accumulator
	addrmodeAbs                     // Absolute
	addrmodeAbsX                    // Absolute, X indexed
	addrmodeAbsY                    // Absolute, Y indexed
	addrmodeImm                     // Immediate
	addrmodeImp                     // Implied
	addrmodeAbsInd                  // (Absolute) Indirect
	addrmodeZpXInd                  // (Zeropage) X indexed indirect
	addrmodeZpIndY                  // (Zeropage) indirect Y indexed
	addrmodeRel                     // Relative
	addrmodeZp                      // Zeropage
	addrmodeZpX                     // Zeropage, X indexed
	addrmodeZpY                     // Zeropage, Y indexed
)

type operand interface {
	read(ctx *clientContext) (uint8, error)
	write(ctx *clientContext, v uint8) error
	readModifyWrite(ctx *clientContext, f func(uint8) uint8) error
}

// Relative --------------------------------------------------------------------
type relOperand struct{ rel uint8 }

func getRelOperand(op operand) relOperand {
	rel, ok := op.(relOperand)
	if !ok {
		panic("invalid cast")
	}
	return rel
}
func (op relOperand) read(ctx *clientContext) (uint8, error) {
	panic("attempted to read/write on an relative operand")
}
func (op relOperand) write(ctx *clientContext, v uint8) error {
	panic("attempted to read/write on an relative operand")
}
func (op relOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
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
func (op impOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
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
func (op immOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
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
func (op accOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
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
func (op absOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := op.addr
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
	v = f(v)
	return ctx.writeMemB(addr, v)
}

// Absolute, X indexed --------------------------------------------------------------------
type absXOperand struct{ addr uint16 }

func (op absXOperand) getAddr(ctx *clientContext, isWrite bool) uint16 {
	addrH16 := ((op.addr & 0xff00) >> 8)
	addrL16 := (op.addr & 0xff) + uint16(ctx.regX)
	isPageCross := (addrL16 & 0xff00) != 0
	if isPageCross || isWrite {
		addrL16 &= 0xff
		ctx.readMemB(addrL16 | (addrH16 << 8)) // Dummy read
		if isPageCross {
			addrH16++
		}
	}
	return addrL16 | (addrH16 << 8)
}
func (op absXOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(op.getAddr(ctx, false))
}
func (op absXOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(op.getAddr(ctx, true), v)
}
func (op absXOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := op.getAddr(ctx, true)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
		ctx.readMemB(addrL16 | (addrH16 << 8)) // Dummy read
		if isPageCross {
			addrH16++
		}
	}
	return addrL16 | (addrH16 << 8)
}
func (op absYOperand) read(ctx *clientContext) (uint8, error) {
	return ctx.readMemB(op.getAddr(ctx, false))
}
func (op absYOperand) write(ctx *clientContext, v uint8) error {
	return ctx.writeMemB(op.getAddr(ctx, true), v)
}
func (op absYOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := op.getAddr(ctx, true)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
func (op absIndOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
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
func (op zpOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := uint16(op.addr)
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
func (op zpXOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := op.getAddr(ctx)

	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
func (op zpYOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr := op.getAddr(ctx)

	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
func (op zpXIndOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr, err := op.getAddr(ctx)
	if err != nil {
		return err
	}
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
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
func (op zpIndYOperand) readModifyWrite(ctx *clientContext, f func(uint8) uint8) error {
	addr, err := op.getAddr(ctx, true)
	if err != nil {
		return err
	}
	v, err := ctx.readMemB(addr)
	if err != nil {
		return err
	}
	ctx.writeMemB(addr, v)
	v = f(v)
	return ctx.writeMemB(addr, v)
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
	// EOR ---------------------------------------------------------------------
	instrs[0x49] = &instr{"eor", eorExec, addrmodeImm}
	instrs[0x45] = &instr{"eor", eorExec, addrmodeZp}
	instrs[0x55] = &instr{"eor", eorExec, addrmodeZpX}
	instrs[0x4d] = &instr{"eor", eorExec, addrmodeAbs}
	instrs[0x5d] = &instr{"eor", eorExec, addrmodeAbsX}
	instrs[0x59] = &instr{"eor", eorExec, addrmodeAbsY}
	instrs[0x41] = &instr{"eor", eorExec, addrmodeZpXInd}
	instrs[0x51] = &instr{"eor", eorExec, addrmodeZpIndY}
	// ORA ---------------------------------------------------------------------
	instrs[0x09] = &instr{"ora", oraExec, addrmodeImm}
	instrs[0x05] = &instr{"ora", oraExec, addrmodeZp}
	instrs[0x15] = &instr{"ora", oraExec, addrmodeZpX}
	instrs[0x0d] = &instr{"ora", oraExec, addrmodeAbs}
	instrs[0x1d] = &instr{"ora", oraExec, addrmodeAbsX}
	instrs[0x19] = &instr{"ora", oraExec, addrmodeAbsY}
	instrs[0x01] = &instr{"ora", oraExec, addrmodeZpXInd}
	instrs[0x11] = &instr{"ora", oraExec, addrmodeZpIndY}
	// LDA ---------------------------------------------------------------------
	instrs[0xa9] = &instr{"lda", ldaExec, addrmodeImm}
	instrs[0xa5] = &instr{"lda", ldaExec, addrmodeZp}
	instrs[0xb5] = &instr{"lda", ldaExec, addrmodeZpX}
	instrs[0xad] = &instr{"lda", ldaExec, addrmodeAbs}
	instrs[0xbd] = &instr{"lda", ldaExec, addrmodeAbsX}
	instrs[0xb9] = &instr{"lda", ldaExec, addrmodeAbsY}
	instrs[0xa1] = &instr{"lda", ldaExec, addrmodeZpXInd}
	instrs[0xb1] = &instr{"lda", ldaExec, addrmodeZpIndY}
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
	// STX ---------------------------------------------------------------------
	instrs[0x86] = &instr{"stx", stxExec, addrmodeZp}
	instrs[0x96] = &instr{"stx", stxExec, addrmodeZpY}
	instrs[0x8e] = &instr{"stx", stxExec, addrmodeAbs}
	// STY ---------------------------------------------------------------------
	instrs[0x84] = &instr{"sty", styExec, addrmodeZp}
	instrs[0x94] = &instr{"sty", styExec, addrmodeZpX}
	instrs[0x8c] = &instr{"sty", styExec, addrmodeAbs}
	// JMP ---------------------------------------------------------------------
	instrs[0x4c] = &instr{"jmp", jmpExec, addrmodeAbs}
	instrs[0x6c] = &instr{"jmp", jmpExec, addrmodeAbsInd}
	// JSR ---------------------------------------------------------------------
	instrs[0x20] = &instr{"jsr", jsrExec, addrmodeAbs}
	// Branch instructions -----------------------------------------------------
	instrs[0xb0] = &instr{"bcs", bcsExec, addrmodeRel}
	instrs[0x90] = &instr{"bcc", bccExec, addrmodeRel}
	instrs[0xf0] = &instr{"beq", beqExec, addrmodeRel}
	instrs[0xd0] = &instr{"bne", bneExec, addrmodeRel}
	instrs[0x10] = &instr{"bpl", bplExec, addrmodeRel}
	instrs[0x30] = &instr{"bmi", bmiExec, addrmodeRel}
	instrs[0x70] = &instr{"bvs", bvsExec, addrmodeRel}
	instrs[0x50] = &instr{"bvc", bvcExec, addrmodeRel}
	// Instructions with implied operands --------------------------------------
	instrs[0xea] = &instr{"nop", nopExec, addrmodeImp}
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
	instrs[0x68] = &instr{"pla", plaExec, addrmodeImp}
	instrs[0x28] = &instr{"plp", plpExec, addrmodeImp}
	instrs[0x40] = &instr{"rti", rtiExec, addrmodeImp}
	instrs[0x60] = &instr{"rts", rtsExec, addrmodeImp}
	instrs[0xaa] = &instr{"tax", taxExec, addrmodeImp}
	instrs[0xa8] = &instr{"tax", tayExec, addrmodeImp}
	instrs[0xba] = &instr{"tax", tsxExec, addrmodeImp}
	instrs[0x8a] = &instr{"tax", txaExec, addrmodeImp}
	instrs[0x9a] = &instr{"tax", txsExec, addrmodeImp}
	instrs[0x98] = &instr{"tax", tyaExec, addrmodeImp}
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
	rel := getRelOperand(op)
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

// ASL -------------------------------------------------------------------------
func aslExec(ctx *clientContext, op operand) error {
	op.readModifyWrite(ctx, func(old uint8) uint8 {
		res := old << 1
		oldBit7 := (old & 0x80) != 0
		ctx.setNZ(res)
		ctx.flagC = oldBit7
		return res
	})
	return nil
}

// LSR -------------------------------------------------------------------------
func lsrExec(ctx *clientContext, op operand) error {
	op.readModifyWrite(ctx, func(old uint8) uint8 {
		res := old >> 1
		oldBit0 := (old & 0x1) != 0
		ctx.setNZ(res)
		ctx.flagC = oldBit0
		return res
	})
	return nil
}

// ROL -------------------------------------------------------------------------
func rolExec(ctx *clientContext, op operand) error {
	op.readModifyWrite(ctx, func(old uint8) uint8 {
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
	return nil
}

// ROR -------------------------------------------------------------------------
func rorExec(ctx *clientContext, op operand) error {
	op.readModifyWrite(ctx, func(old uint8) uint8 {
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
	return nil
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

// JMP -------------------------------------------------------------------------
func jmpExec(ctx *clientContext, op operand) error {
	if absOp, ok := op.(absOperand); ok {
		ctx.regPC = absOp.addr
	} else if indOp, ok := op.(absIndOperand); ok {
		newPcL, err := ctx.readMemB(indOp.addr)
		if err != nil {
			return err
		}
		newPcH, err := ctx.readMemB(
			// We emulate 6502's indirect JMP bug (= we preserve upper 8-bit of the address after adding 1 to it)
			((indOp.addr + 1) & 0xffff & ^uint16(0xff00)) | (indOp.addr & 0xff00))
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

// ADC -------------------------------------------------------------------------
func adcImpl(lhs, rhs uint8, carryIn bool) (res uint8, carryOut bool, overflow bool) {
	carryVal := uint8(0)
	if carryIn {
		carryVal = 1
	}
	result16 := uint16(lhs + rhs + carryVal)
	carryOut = (result16 & 0xff00) != 0
	res = uint8(result16)
	overflow =
		((lhs^rhs)&0x80 == 0) && // It's overflow if LHS and RHS signs are the same
			((lhs^res)&0x80 != 0) // and resulting sign is different
	return
}

// Instructions with implied operands ------------------------------------------
func nopExec(ctx *clientContext, op operand) error {
	return nil
}
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
