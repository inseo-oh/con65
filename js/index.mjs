import fs from 'node:fs/promises';
import path from 'node:path';
import CPUClient from './cpu.mjs';

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

function hex(x) {
    return x.toString(16);
}

cpu.onTraceExec = (pc, ir, disasm) => {
    execLogs.push(` EXEC | pc=${hex(pc)} ir=${hex(ir)} ${disasm}`);
};
cpu.onBusWrite = (addr, val) => {
    execLogs.push(`  BUS | W addr=${hex(addr)} val=${hex(val)}`);
    ram[addr] = val;
};
cpu.onBusRead = (addr) => {
    let val = ram[addr];
    execLogs.push(`  BUS | R addr=${hex(addr)} val=${hex(val)}`);
    return val;
};

let passCount = 0;
let failCount = 0;
let failMismatchCount = 0;

async function runTestFile(filename, fileText) {
    const tests = JSON.parse(fileText);

    for (const test of tests) {
        execLogs = [];
        const initial = test.initial;
        const final = test.final;
        const loadPromises = [];

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
            compareReg('p', final.p, () => cpu.readP())
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
        `TEST FINISHED: ${passCount} passed, ${failCount} failed(${failMismatchCount} mismatches)`
    );
    cpu.bye();

    const took = Math.floor((new Date() - beginTime) / 1000);
    const mins = Math.floor(took / 60);
    const secs = took % 60;
    console.log(`Tests took ${mins} minutes ${secs} seconds`);
});
