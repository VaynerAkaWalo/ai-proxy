import { afterEach, expect, test } from "bun:test";
import { join } from "node:path";

const root = join(import.meta.dir, "..");
let server: ReturnType<typeof Bun.serve> | undefined;

afterEach(() => server?.stop(true));

test("subscriptions use the reconciled inventory instead of historical quota accounts", async () => {
  const definition = await Bun.file(join(root, "dashboards/llm-usage.json")).json();
  const widget = definition.sections
    .flatMap((section: { widgets: { id: string }[] }) => section.widgets)
    .find((widget: { id: string }) => widget.id === "subscriptions");

  expect(widget.query.where).toEqual([
    { dimension: "quota_window", op: "eq", value: "subscription" },
  ]);
  expect(widget.window).toBe("latest");
});

async function apply(status = 200, token = "dashboard-owner-token") {
  const requests: { path: string; headers: Headers; body: unknown }[] = [];

  server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      requests.push({
        path: new URL(request.url).pathname,
        headers: request.headers,
        body: await request.json(),
      });

      return Response.json(
        { result: "test response" },
        { status, headers: { location: "/redirected" } },
      );
    },
  });

  const process = Bun.spawn(["bash", join(root, "deploy/apply-analytics-dashboard.sh")], {
    cwd: "/tmp",
    env: {
      ...Bun.env,
      ANALYTICS_URL: server.url.href,
      ANALYTICS_DASHBOARD_TOKEN: token,
    },
    stdout: "pipe",
    stderr: "pipe",
  });

  const [exitCode, stdout, stderr] = await Promise.all([
    process.exited,
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
  ]);

  return { exitCode, stdout, stderr, requests };
}

for (const status of [200, 201]) {
  test(`applies versioned layout for HTTP ${status}`, async () => {
    const definition = await Bun.file(join(root, "dashboards/llm-usage.json")).json();
    const result = await apply(status);

    expect(result.exitCode).toBe(0);
    expect(result.requests).toHaveLength(1);

    const [request] = result.requests;
    expect(request!.path).toBe("/api/dashboards/llm-usage");
    expect(request!.headers.get("authorization")).toBe("Bearer dashboard-owner-token");
    expect(request!.headers.get("content-type")).toBe("application/json");
    expect(request!.body).toEqual(definition);
    expect(definition).not.toHaveProperty("visibility");
    expect(definition).not.toHaveProperty("editable");
  });
}

for (const status of [302, 401, 403, 422, 500]) {
  test(`rejects HTTP ${status} without retrying or following redirects`, async () => {
    const result = await apply(status);

    expect(result.exitCode).not.toBe(0);
    expect(result.requests).toHaveLength(1);
    expect(result.stdout + result.stderr).not.toContain("dashboard-owner-token");
  });
}

test("requires explicit dashboard credentials before sending", async () => {
  const result = await apply(200, "");

  expect(result.exitCode).not.toBe(0);
  expect(result.stderr).toContain("ANALYTICS_DASHBOARD_TOKEN");
  expect(result.requests).toHaveLength(0);
});
