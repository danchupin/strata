import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

// Tiny shell-out wrapper used by multi-replica.spec.ts to drive lifecycle
// transitions on the lab-tikv compose containers. No mocking — the spec
// assumes real docker is available on the host (CI sets up the lab via
// `make up-lab-tikv`). execFile (not exec) avoids shell quoting surprises
// around container names.

const exec = promisify(execFile);

export async function dockerStop(name: string): Promise<void> {
  await exec('docker', ['stop', name], { timeout: 30_000 });
}

export async function dockerStart(name: string): Promise<void> {
  await exec('docker', ['start', name], { timeout: 30_000 });
}

export async function dockerKill(name: string): Promise<void> {
  await exec('docker', ['kill', name], { timeout: 30_000 });
}
