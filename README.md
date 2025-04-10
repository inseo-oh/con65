# con65
This is proof-of-concept 65C02 emulator over network. The emulated CPU is provided as a TCP server, and clients connect to the server to use the CPU.

Clients can send commands to read/write CPU registers, and optionally enable tracing to get live disassembly from server as it executes code. And during execution, server will ask client for any bus accesses.

This is *very experimental* project, and currently only test client is provided.

## Usage

### Running the server

The server supports raw TCP sockets, as well as WebSocket. In the latter case it doubles as simple HTTP server that serves any content under `www` directory.
- TCP: `go run . --mode=tcp`
- WebSockets: `go run . --mode=ws`

(NOTE: The default port for both mode is **6502**)

But be aware that while Websocket is a extra layer that works on top of TCP, so it's *slower* than raw TCP mode.

By default the server listens on localhost address, meaning it won't expose the server to the network. To change the address(and port), use `--addr` option. For example, to listen on any network interface:
```shell
go run . --mode=tcp --addr=0.0.0.0:6502
```

### Running the test client

The test client requires Node.JS, and was tested with Node.JS v22.11.0.
To run the test client, you first need [test JSON files](https://github.com/SingleStepTests/65x02).

Then run the client like this:
```shell
node www/json_tester/index.js path/to/the/json 
```

For example following will run JSR test, assuming you cloned the JSON test files repo at `65x02`:
```shell
node www/json_tester/index.js 65x02/wdc65c02/v1/20.json
```

You can also run all JSONs in a directory. Following will run ALL 65c02 tests:
```shell
node www/json_tester/index.js 65x02/wdc65c02/v1/
```

NOTE: Test client connects via raw TCP by default. To use Websocket, use `--use-websocket` flag.

Takes approx. 9~10 minutes to complete on i3-10100 running under WSL, or 13 minutes when using Websocket.

## TODOs
- Fix cycle times
- BCD arithmetics (ADC and SBC with D=1)
- BBR and BBS (Implemented but seems to be broken)

- NOP 0x5C passes test, but [the test seems to have incorrect bus cycle count](https://github.com/SingleStepTests/65x02/issues/12)