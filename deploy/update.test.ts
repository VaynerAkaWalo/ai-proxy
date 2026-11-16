import { afterEach, beforeEach, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const deployDir = import.meta.dir;

const stubs = {
  docker: `#!/usr/bin/env bash
if [ "$1 $2" = "image inspect" ]; then
  count=$(cat "$CALLS/inspects" 2>/dev/null || echo 0)
  echo $((count + 1)) > "$CALLS/inspects"
  echo "$IMAGE_IDS" | cut -d' ' -f$((count + 1))
fi
if [ "$1" = "pull" ] && [ -n "\${PULL_FAILS:-}" ]; then
  exit 1
fi
`,
  systemctl: `#!/usr/bin/env bash
echo "$@" >> "$CALLS/systemctl"
`,
  oci: `#!/usr/bin/env bash
printf %s "vault-token" | base64
`,
  curl: `#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case "$1" in
    -d) printf '%s\\n' "$2" | jq -c . >> "$CALLS/payloads"; shift 2;;
    -H) printf '%s\\n' "$2" >> "$CALLS/headers"; shift 2;;
    *) shift;;
  esac
done
`,
};

let sandbox: string;

beforeEach(async () => {
  sandbox = await mkdtemp(join(tmpdir(), "update-test-"));
  await mkdir(join(sandbox, "bin"));

  for (const [name, script] of Object.entries(stubs)) {
    const path = join(sandbox, "bin", name);
    await writeFile(path, script);
    await chmod(path, 0o755);
  }
});

afterEach(() => rm(sandbox, { recursive: true, force: true }));

async function update(env: Record<string, string>) {
  const process = Bun.spawn(["bash", join(deployDir, "update.sh")], {
    env: {
      ...Bun.env,
      PATH: `${join(sandbox, "bin")}:${Bun.env.PATH}`,
      CALLS: sandbox,
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });

  const exitCode = await process.exited;
  const payloads = await readLines("payloads");
  const headers = await readLines("headers");
  const restarts = await readLines("systemctl");

  return { exitCode, payloads, headers, restarts };
}

async function readLines(name: string) {
  const content = await readFile(join(sandbox, name), "utf8").catch(() => "");

  return content.split("\n").filter(Boolean);
}

test("reports the digest it now runs when the pulled image changed", async () => {
  const result = await update({ IMAGE_IDS: "sha256:old0123456789abcdef sha256:new0123456789abcdef" });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
  expect(result.headers).toContain("authorization: Bearer vault-token");

  const [deploy] = result.payloads.map((line) => JSON.parse(line));
  expect(deploy).toMatchObject({
    job: "ai-proxy-deploy",
    status: "success",
    message: "now running sha256:new0123456789",
  });
});

test("reports nothing when the pulled image is the one already running", async () => {
  const result = await update({ IMAGE_IDS: "sha256:same sha256:same" });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual([]);
  expect(result.payloads).toEqual([]);
});

test("still updates when changefeed cannot be reached", async () => {
  await writeFile(join(sandbox, "bin", "curl"), "#!/usr/bin/env bash\nexit 7\n");

  const result = await update({ IMAGE_IDS: "sha256:old sha256:new" });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
});

test("fails without restarting when the pull fails", async () => {
  const result = await update({ IMAGE_IDS: "sha256:old sha256:new", PULL_FAILS: "1" });

  expect(result.exitCode).not.toBe(0);
  expect(result.restarts).toEqual([]);
});
