// Copyright (c) 2025, Oh Inseo (YJK) -- Licensed under BSD-2-Clause
import fs from 'node:fs/promises';
import path from 'node:path';
import CPUClient from './cpu.mjs';

const SKIP_DECIMAL_TESTS = false;
const TEST_TIME_LIMIT = 1; // Seconds
const servAddr = '127.0.0.1';
let testPaths = [];

for (let i = 2; i < process.argv.length; i++) {
    testPaths.push(process.argv[i]);
}
if (testPaths.length === 0) {
    console.error(
        `Usage: node js/index.mjs <list of test JSON files/directories>`
    );
    console.error(
        `For the test files, get it from https://github.com/SingleStepTests/65x02`
    );
    process.exit(1);
}

const ram = new Uint8Array(16 * 1024 * 1024);
const cpu = new CPUClient();
let execLogs = [];
let expectedCycles = [];
let busFail = false;

function hex(x) {
    return x.toString(16);
}

function cycleToBusLogFormat(cycAddr, cycVal, cycTyp) {
    const typ = cycTyp === 'read' ? 'R' : 'W';
    return `${typ} addr=${hex(cycAddr)} val=${hex(cycVal)}`;
}

cpu.onTraceExec = (pc, ir, disasm) => {
    execLogs.push(` EXEC | pc=${hex(pc)} ir=${hex(ir)} ${disasm}`);
};
cpu.onBusWrite = (addr, val) => {
    ram[addr] = val;
    let badReason = '';
    if (expectedCycles.length === 0) {
        badReason = 'Too many cycles';
    } else {
        const [cycAddr, cycVal, cycTyp] = expectedCycles.shift();
        if (cycTyp != 'write' || cycAddr != addr || cycVal != val) {
            const s = cycleToBusLogFormat(cycAddr, cycVal, cycTyp);
            badReason = `Expected ${s}`;
        }
    }
    if (badReason.length !== 0) {
        busFail = true;
    }
    let busLog = `  BUS | W addr=${hex(addr)} val=${hex(val)}`;
    if (busFail) {
        busLog += `(BAD: ${badReason})`;
    }
    execLogs.push(busLog);
};
cpu.onBusRead = (addr) => {
    let val = ram[addr];
    let badReason = '';
    if (expectedCycles.length === 0) {
        badReason = 'Too many cycles';
    } else {
        const [cycAddr, cycVal, cycTyp] = expectedCycles.shift();
        if (cycTyp != 'read' || cycAddr != addr || cycVal != val) {
            const s = cycleToBusLogFormat(cycAddr, cycVal, cycTyp);
            badReason = `Expected ${s}`;
        }
    }
    if (badReason.length !== 0) {
        busFail = true;
    }
    let busLog = `  BUS | R addr=${hex(addr)} val=${hex(val)}`;
    if (busFail) {
        busLog += `(BAD: ${badReason})`;
    }
    execLogs.push(busLog);
    return val;
};
cpu.onStop = () => {
    execLogs.push(` STOP |`);
}

let passCount = 0;
let failCount = 0;
let skipCount = 0;
let failMismatchCount = 0;

async function runTestFile(filename, fileText) {
    console.log(`# ${filename}`)
    if (fileText.trim().length === 0) {
        console.log(`=> File is empty`)
        return;
    }
    const tests = JSON.parse(fileText);

    for (const test of tests) {
        if (SKIP_DECIMAL_TESTS) {
            if (test.initial.p & (1 << 3)) {
                skipCount++;
                continue;
            }
        }

        execLogs = [];
        const initial = test.initial;
        const final = test.final;
        const loadPromises = [];
        expectedCycles = structuredClone(test.cycles);
        busFail = false;

        //------------------------------------------------------------------
        // Setup initial state
        //------------------------------------------------------------------
        loadPromises.push(
            cpu.writePc(initial.pc),
            cpu.writeS(initial.s),
            cpu.writeA(initial.a),
            cpu.writeX(initial.x),
            cpu.writeY(initial.y),
            cpu.writeP(initial.p)
        );

        // Clear destination RAM -------------------------------------------
        for (const [addr, _] of final.ram) {
            ram[addr] = 0;
        }

        // Load RAM contents -----------------------------------------------
        for (const [addr, val] of initial.ram) {
            ram[addr] = val;
        }

        //------------------------------------------------------------------
        // Run the CPU
        //------------------------------------------------------------------
        let failed = false;

        try {
            await Promise.all(loadPromises);
        } catch (e) {
            console.log('State load error! Skipping this test...');
            console.log(e);
            failCount++;
            continue;
        }
        let runStartTime = new Date();
        const finalPc = final.pc;
        while (true) {
            const elapsed = new Date() - runStartTime;
            if (TEST_TIME_LIMIT * 1000 <= elapsed) {
                console.log(
                    `>>> Execution timeout (Expected final PC: ${hex(finalPc)})`
                );
                failed = true;
                break;
            }
            try {
                await cpu.tick();
            } catch (e) {
                console.log('>>> CPU Error');
                console.log(e);
                failed = true;
                break;
            }
            const pc = await cpu.readPc();
            if (pc === finalPc) {
                break;
            }
        }

        if (busFail) {
            failed = true;
        }

        //------------------------------------------------------------------
        // See if there are remaining memory cycles
        //------------------------------------------------------------------
        let remainingCyclesLog = [];
        for (const c of expectedCycles) {
            remainingCyclesLog.push(cycleToBusLogFormat(...c));
            failed = true;
        }

        //------------------------------------------------------------------
        // Compare the final state
        //------------------------------------------------------------------

        let mismatchLog = [];
        const onMismatch = (name, expect, got) => {
            mismatchLog.push(
                `${name}: Expected ${hex(expect)}, Got ${hex(got)}`
            );
            failed = true;
        };
        const compareReg = async (regName, expect, readReg) => {
            expect = expect;
            const got = await readReg();
            if (expect !== got) {
                onMismatch(regName, expect, got);
            }
        };
        const comparePromises = [];

        // Compare registers ---------------------------------------------------
        comparePromises.push(
            compareReg('pc', final.pc, () => cpu.readPc()),
            compareReg('s', final.s, () => cpu.readS()),
            compareReg('a', final.a, () => cpu.readA()),
            compareReg('x', final.x, () => cpu.readX()),
            compareReg('y', final.y, () => cpu.readY()),
            compareReg('p', (final.p & 0xcf) | 0x20, () => cpu.readP())
        );

        try {
            await Promise.all(comparePromises);
        } catch (e) {
            console.log('Compare error!');
            console.log(e);
            failed = true;
            break;
        }

        // Compare RAM contents --------------------------------------------
        for (const [addr, expect] of final.ram) {
            const got = ram[addr];
            if (got != expect) {
                onMismatch(`Memory@${hex(addr)}`, expect, got);
            }
        }

        //------------------------------------------------------------------
        // Show the report
        //------------------------------------------------------------------

        if (failed) {
            console.log(`[${filename}] ${test.name}`);
            if (remainingCyclesLog.length !== 0) {
                console.log(
                    `:: Remaining cycles (${remainingCyclesLog.length}):`
                );
                for (const line of remainingCyclesLog) {
                    console.log(`>>> ${line}`);
                }
            }
            if (mismatchLog.length !== 0) {
                console.log(`:: Mismatches (${mismatchLog.length}):`);
                for (const line of mismatchLog) {
                    console.log(`>>> ${line}`);
                }
                failMismatchCount++;
            }
            console.log(`:: Execution log (${execLogs.length}):`);
            for (const line of execLogs) {
                console.log(`>>> ${line}`);
            }
            failCount++;
        } else {
            passCount++;
        }
    }
}

let filepaths = [];

for (const p of testPaths) {
    if ((await fs.stat(p)).isDirectory()) {
        filepaths = (await fs.readdir(p)).map((x) => path.join(p, x));
    } else {
        filepaths.push(p);
    }
}

cpu.connect(servAddr, async () => {
    await cpu.setTraceExec(true);

    const beginTime = new Date();
    for (const file of filepaths) {
        if (!file.endsWith('.json')) {
            continue;
        }
        const fileText = (await fs.readFile(file)).toString();
        await runTestFile(file, fileText);
    }
    console.log(
        `TEST FINISHED: ${passCount} passed, ${failCount} failed(${failMismatchCount} mismatches), ${skipCount} skipped`
    );
    cpu.bye();

    const took = Math.floor((new Date() - beginTime) / 1000);
    const mins = Math.floor(took / 60);
    const secs = took % 60;
    console.log(`Tests took ${mins} minutes ${secs} seconds`);
});
