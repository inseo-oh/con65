import net from 'node:net';

const SERVER_PORT = 6502;

export default class CPUClient {
    #client = undefined;
    #inboxBuf = [];
    #sentBytesSum = 0;
    #recvBytesSum = 0;
    #connStartTime = undefined;

    static CCR_FLAG_C = 1 << 0;
    static CCR_FLAG_V = 1 << 1;
    static CCR_FLAG_Z = 1 << 2;
    static CCR_FLAG_N = 1 << 3;
    static CCR_FLAG_X = 1 << 4;

    // Takes address, and returns boolean indicating whether a valid device is there or not.
    onAddressAsserted = (_addr) => {
        throw new Error('not implemented');
    };
    // ReadA from the last asserted address, and returns the result.
    onBusRead = (_ds) => {
        throw new Error('not implemented');
    };
    // Writes to the last asserted address.
    onBusWrite = () => {
        throw new Error('not implemented');
    };
    // RESET signal was asserted
    onResetAsserted = () => {
        throw new Error('not implemented');
    };
    // Called if execution tracing is enabled
    onTraceExec = (_pc, _ir, _disasm) => {
        throw new Error('not implemented');
    };
    // Called if exception tracing is enabled
    // Parameters ir, errAddr, errFlags are not set if it's not a memory exception(i.e. Bus and Address error).
    onTraceExc = (_exc, _pc, _ir, _errAddr, _errFlags) => {
        throw new Error('not implemented');
    };

    constructor() {}

    // Queue for functions waiting for response
    #responseWaitQueue = [];

    connect(host, onConnected) {
        this.#client = net.createConnection({ host, port: SERVER_PORT }, () => {
            console.log('[CPUClient] Connected');
            onConnected();
            this.#connStartTime = new Date();
        });
        this.#client.on('end', () => {
            const secs = (new Date() - this.#connStartTime) / 1000;
            const sentStr = `${Math.floor(
                this.#sentBytesSum / secs
            )} bytes/sec`;
            const recvStr = `${Math.floor(
                this.#recvBytesSum / secs
            )} bytes/sec`;
            console.log(
                `[CPUClient] Disconnected - Sent ${sentStr}, Recv ${recvStr}`
            );
        });
        this.#client.on('data', (data) => {
            this.#recvBytesSum += data.length;
            for (let i = 0; i < data.length; i++) {
                this.#inboxBuf.push(data.readUint8(i));
            }
            while (true) {
                if (this.#inboxBuf.length === 0) {
                    break;
                }
                // Look at the first response byte to see what it means.
                // - If it's a message we can't understand, remove it and move to next one.
                // - If it's a message we can understand but need more data, exit the event handler.
                //   Then check it again next time we receive some more data.
                const tp = this.#inboxBuf[0];
                switch (tp) {
                    case netOpbyteAck: {
                        if (this.#responseWaitQueue.length === 0) {
                            console.error(
                                'Got ACK but there are no requests...?'
                            );
                            this.#takeMsg('');
                            break;
                        }
                        const [callback, fmt] = this.#responseWaitQueue[0];
                        const res = this.#takeMsg(fmt);
                        if (res === undefined) {
                            // Try again next time
                            return;
                        }
                        callback('ok', res);
                        this.#responseWaitQueue.shift();
                        break;
                    }

                    case netOpbyteFail: {
                        if (this.#responseWaitQueue.length === 0) {
                            console.error(
                                'Got FAIL but there are no requests...?'
                            );
                            this.#takeMsg('');
                            break;
                        }
                        const [callback, fmt] = this.#responseWaitQueue[0];
                        callback('error');
                        this.#responseWaitQueue.shift();
                        break;
                    }

                    case netOpbyteEventReadBus: {
                        const res = this.#takeMsg('w');
                        if (res === undefined) {
                            // Try again next time
                            return;
                        }
                        const [addr] = res;
                        const val = this.onBusRead(addr);
                        this.#client.write(new Uint8Array([netOpbyteAck, val]));
                        break;
                    }

                    case netOpbyteEventWriteBus: {
                        const res = this.#takeMsg('wb');
                        if (res === undefined) {
                            // Try again next time
                            return;
                        }
                        const [addr, val] = res;
                        this.onBusWrite(addr, val);
                        this.#client.write(new Uint8Array([netOpbyteAck]));
                        break;
                    }

                    case netOpbyteEventTraceExec: {
                        const res = this.#takeMsg('wbs');
                        if (res === undefined) {
                            // Try again next time
                            return;
                        }
                        const [pc, ir, disasm] = res;
                        this.onTraceExec(pc, ir, disasm);
                        this.#client.write(new Uint8Array([netOpbyteAck]));
                        break;
                    }

                    default: {
                        throw Error(`Unrecognized opbyte ${tp.toString(16)}`);
                    }
                }
            }
        });
    }

    bye() {
        this.#client.end(new Uint8Array([netOpbyteBye]));
    }

    async setTraceExec(v) {
        const cmd = [v ? netOpbyteTraceExecOn : netOpbyteTraceExecOff];
        return this.#sendCmd(cmd, '');
    }

    async tick() {
        const cmd = [netOpbyteTick];
        return this.#sendCmd(cmd, '');
    }

    async writeA(val) {
        const cmd = [netOpbyteWriteA, val];
        return this.#sendCmd(cmd, '');
    }

    async readA() {
        const cmd = [netOpbyteReadA];
        return (await this.#sendCmd(cmd, 'b'))[0];
    }

    async writeX(val) {
        const cmd = [netOpbyteWriteX, val];
        return this.#sendCmd(cmd, '');
    }

    async readX() {
        const cmd = [netOpbyteReadX];
        return (await this.#sendCmd(cmd, 'b'))[0];
    }

    async writeY(val) {
        const cmd = [netOpbyteWriteY, val];
        return this.#sendCmd(cmd, '');
    }

    async readY() {
        const cmd = [netOpbyteReadY];
        return (await this.#sendCmd(cmd, 'b'))[0];
    }

    async writeP(val) {
        const cmd = [netOpbyteWriteP, val];
        return this.#sendCmd(cmd, '');
    }

    async readP() {
        const cmd = [netOpbyteReadP];
        return (await this.#sendCmd(cmd, 'b'))[0];
    }

    async writeS(val) {
        const cmd = [netOpbyteWriteS, val];
        return this.#sendCmd(cmd, '');
    }

    async readS() {
        const cmd = [netOpbyteReadS];
        return (await this.#sendCmd(cmd, 'b'))[0];
    }

    async writePc(val) {
        const cmd = [netOpbyteWritePc, ...makeW(val)];
        return this.#sendCmd(cmd, '');
    }

    async readPc() {
        const cmd = [netOpbyteReadPc];
        return (await this.#sendCmd(cmd, 'w'))[0];
    }

    #sendCmd(cmd, fmt) {
        cmd.forEach((e) => {
            if (typeof e !== 'number') {
                throw TypeError(`${e} is not a number`);
            }
        });
        this.#client.write(new Uint8Array(cmd));
        this.#sentBytesSum += cmd.length;
        return new Promise((resolve, reject) => {
            let o = [
                (status, data) => {
                    clearTimeout(timeout);
                    if (status === 'ok') {
                        resolve(data);
                    } else {
                        reject(
                            new Error(
                                `Server returned FAIL response(Command: ${cmdName})`
                            )
                        );
                    }
                },
                fmt,
            ];
            const opbyteStr = cmd[0].toString(16);
            let timeout = setTimeout(() => {
                o[0] = (status, data) => {
                    console.error(
                        `Response arrived too late(Command: ${opbyteStr}). status=${status}, data=${data}`
                    );
                };
                reject(new Error(`Response timeout(Command: ${opbyteStr})`));
            }, 1000);
            this.#responseWaitQueue.push(o);
        });
    }

    // Returns undefined if buffered data is not sufficient yet.
    #takeMsg(fmt) {
        // Check if we have enough data buffered.
        let neededLen = 1; // Length of type byte
        for (let i = 0; i < fmt.length; i++) {
            switch (fmt[i]) {
                case 'b':
                    neededLen += 1;
                    break;
                case 'w':
                    neededLen += 2;
                    break;
                case 'l':
                    neededLen += 4;
                    break;
                case 's': {
                    if (this.#inboxBuf.length < 2) {
                        return;
                    }
                    const len = this.#inboxBuf[neededLen - 1];
                    neededLen += 1 + len;
                    break;
                }
                default:
                    console.error(`Unrecognized format char ${fmt[i]}`);
                    break;
            }
        }
        if (this.#inboxBuf.length < neededLen) {
            return undefined;
        }

        // Remove the type byte
        this.#inboxBuf.shift();
        // Parse the result
        let results = [];
        for (let i = 0; i < fmt.length; i++) {
            switch (fmt[i]) {
                case 'b':
                    results.push(this.#inboxBuf.shift());
                    break;
                case 'w': {
                    const bytes = [
                        this.#inboxBuf.shift(),
                        this.#inboxBuf.shift(),
                    ];
                    results.push((bytes[0] << 8) | bytes[1]);
                    break;
                }
                case 'l': {
                    const bytes = [
                        this.#inboxBuf.shift(),
                        this.#inboxBuf.shift(),
                        this.#inboxBuf.shift(),
                        this.#inboxBuf.shift(),
                    ];
                    results.push(
                        (bytes[0] << 24) |
                            (bytes[1] << 16) |
                            (bytes[2] << 8) |
                            bytes[3]
                    );
                    break;
                }
                case 's': {
                    const len = this.#inboxBuf.shift();
                    const bytes = new Uint8Array(len);
                    for (let i = 0; i < len; i++) {
                        bytes[i] = this.#inboxBuf.shift();
                    }
                    const tdec = new TextDecoder('utf-8');
                    results.push(tdec.decode(bytes));
                    break;
                }
                default:
                    console.error(`Unrecognized format char ${fmt[i]}`);
                    break;
            }
        }
        return results;
    }
}

function makeW(v) {
    return [(v >> 8) & 0xff, v];
}

// 0x - Response type.
// Every response starts with this byte,
const netOpbyteAck = 0x00; // Acknowledged
const netOpbyteFail = 0x01; // Failed

// 1x - General commands
const netOpbyteBye = 0x10; // Close the connection
const netOpbyteTraceExecOn = 0x11; // Trace Execution - Enable
const netOpbyteTraceExecOff = 0x12; // Trace Execution - Disable
const netOpbyteTick = 0x1f; // Run the CPU for a tick

// 2x - CPU state manipulation commands
const netOpbyteWriteA = 0x20; // Accumulator write
const netOpbyteReadA = 0x21; // Accumulator read
const netOpbyteWriteX = 0x22; // X Register write
const netOpbyteReadX = 0x23; // X Register read
const netOpbyteWriteY = 0x24; // Y Register write
const netOpbyteReadY = 0x25; // Y Register read
const netOpbyteWriteS = 0x26; // Stack pointer write
const netOpbyteReadS = 0x27; // Stack pointer read
const netOpbyteWriteP = 0x28; // PC write
const netOpbyteReadP = 0x29; // PC read
const netOpbyteWritePc = 0x2a; // PC write
const netOpbyteReadPc = 0x2b; // PC read

// 8x - Server events
// When client receives one of these, it should respond to it accordingly.
const netOpbyteEventReadBus = 0x80; // Read from address
const netOpbyteEventWriteBus = 0x81; // Write to address
const netOpbyteEventTraceExec = 0x82; // Event for Trace Execution
