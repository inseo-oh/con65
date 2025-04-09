# con65
This is proof-of-concept 65C02 emulator over network. The emulated CPU is provided as a TCP server, and clients connect to the server to use the CPU.

Clients can send commands to read/write CPU registers, and optionally enable tracing to get live disassembly from server as it executes code. And during execution, server will ask client for any bus accesses.

This is very experimental project, and currently only test client is provided.

## Testing

The test client requires Node.JS, and was tested with Node.JS v22.11.0.
To run the test client, you first need [test JSON files](https://github.com/SingleStepTests/65x02).

Then run the client like this:
```shell
node js/index.mjs path/to/the/json 
```

For example following will run JSR test, assuming you cloned the JSON test files repo at `65x02`:
```
node js/index.mjs 65x02/wdc65c02/v1/20.json
```

# TODOs
- Fix cycle times
- BCD arithmetics (ADC and SBC with D=1)
- BBR and BBS (Implemented but seems to be broken)
- TRB and TSB
