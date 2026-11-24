import { afterEach, beforeEach, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const deployDir = import.meta.dir;
const repoDigest = "ghcr.io/vaynerakawalo/ai-proxy@sha256:4f2c9e1ab7d3";

const stubs = {
  docker: `#!/usr/bin/env bash
if [ "$1 $2" = "image inspect" ] && [[ "$*" == *RepoDigests* ]]; then
  [ -n "\${DIGEST_FAILS:-}" ] && exit 1
  echo "$REPO_DIGEST"
elif [ "$1 $2" = "image inspect" ]; then
  count=$(cat "$CALLS/inspects" 2>/dev/null || echo 0)
  echo $((count + 1)) > "$CALLS/inspects"
  echo "$IMAGE_IDS" | cut -d' ' -f$((count + 1))
fi
if [ "$1" = "pull" ]; then
  pulls=$(cat "$CALLS/pulls" 2>/dev/null || echo 0)
  echo $((pulls + 1)) > "$CALLS/pulls"
  if [ -n "\${PULL_FAILS:-}" ] || [ "$pulls" -lt "\${PULL_FAILS_TIMES:-0}" ]; then
    exit 1
  fi
fi
`,
  systemctl: `#!/usr/bin/env bash
echo "$@" >> "$CALLS/systemctl"
`,
  oci: `#!/usr/bin/env bash
[ -n "\${TOKEN_FAILS:-}" ] && exit 23
printf %s "vault-token" | base64
`,
  curl: `#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case "$1" in
    -d) printf '%s\\n' "$2" | jq -c . >> "$CALLS/payloads"; shift 2;;
    -H) [ "$2" = "@-" ] && cat >> "$CALLS/headers"; shift 2;;
    http*) echo "$1" >> "$CALLS/urls"; shift;;
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

async function run(command: string[], env: Record<string, string>) {
  const process = Bun.spawn(command, {
    env: {
      ...Bun.env,
      PATH: `${join(sandbox, "bin")}:${Bun.env.PATH}`,
      CALLS: sandbox,
      REPO_DIGEST: repoDigest,
      AI_PROXY_PULL_BACKOFF_SECONDS: "0",
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });

  const [exitCode, stderr] = await Promise.all([
    process.exited,
    new Response(process.stderr).text(),
  ]);

  return {
    exitCode,
    stderr,
    payloads: (await readLines("payloads")).map((line) => JSON.parse(line)),
    headers: await readLines("headers"),
    restarts: await readLines("systemctl"),
    urls: await readLines("urls"),
    pulls: Number((await readLines("pulls"))[0] ?? 0),
  };
}

const update = (env: Record<string, string>) =>
  run(["bash", join(deployDir, "update.sh")], env);

const updateAsTheUnitRunsIt = (env: Record<string, string>) =>
  run(
    [
      join(deployDir, "with-changefeed-token"),
      join(deployDir, "report-run"),
      "ai-proxy-update",
      "--",
      join(deployDir, "update.sh"),
    ],
    env,
  );

async function readLines(name: string) {
  const content = await readFile(join(sandbox, name), "utf8").catch(() => "");

  return content.split("\n").filter(Boolean);
}

test("reports the deploy with the digest it now runs when the pulled image changed", async () => {
  const result = await update({
    IMAGE_IDS: "sha256:old0123456789abcdef sha256:new0123456789abcdef",
  });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
  expect(result.headers).toEqual(["authorization: Bearer vault-token"]);
  expect(result.urls).toEqual(["https://changefeed.blamedevs.com/deploys"]);
  expect(result.payloads).toMatchObject([
    { service: "ai-proxy", image: repoDigest },
  ]);
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

test("still updates when the digest of the new image cannot be read", async () => {
  const result = await update({
    IMAGE_IDS: "sha256:old sha256:new",
    DIGEST_FAILS: "1",
  });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
  expect(result.payloads).toEqual([]);
});

test("still updates when the unit cannot read the changefeed token from the vault", async () => {
  const result = await updateAsTheUnitRunsIt({
    IMAGE_IDS: "sha256:old sha256:new",
    TOKEN_FAILS: "1",
  });

  expect(result.exitCode).toBe(0);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
  expect(result.stderr).toContain("running unreported");
});

test("reports the update run itself under the unit's job name", async () => {
  const result = await updateAsTheUnitRunsIt({
    IMAGE_IDS: "sha256:old sha256:new",
  });

  expect(result.payloads).toMatchObject([
    { service: "ai-proxy" },
    { job: "ai-proxy-update", status: "success" },
  ]);
});

test("updates when the pull succeeds after transient failures", async () => {
  const result = await update({
    IMAGE_IDS: "sha256:old sha256:new",
    PULL_FAILS_TIMES: "2",
  });

  expect(result.exitCode).toBe(0);
  expect(result.pulls).toBe(3);
  expect(result.restarts).toEqual(["restart ai-proxy.service"]);
});

test("fails without restarting when every pull attempt fails", async () => {
  const result = await update({
    IMAGE_IDS: "sha256:old sha256:new",
    PULL_FAILS: "1",
  });

  expect(result.exitCode).not.toBe(0);
  expect(result.pulls).toBe(3);
  expect(result.restarts).toEqual([]);
});

test("fails the unit and reports the failure when the pull fails", async () => {
  const result = await updateAsTheUnitRunsIt({
    IMAGE_IDS: "sha256:old sha256:new",
    PULL_FAILS: "1",
  });

  expect(result.exitCode).not.toBe(0);
  expect(result.payloads).toMatchObject([
    { job: "ai-proxy-update", status: "failure" },
  ]);
});
