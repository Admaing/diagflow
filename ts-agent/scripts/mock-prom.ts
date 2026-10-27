/**
 * Local mock for Prometheus + Alertmanager, used by `npm run smoke:e2e`.
 * Returns empty Prometheus results and one active alert.
 */
import http from "node:http";

const port = Number(process.env.MOCK_PROM_PORT ?? 19090);

const server = http.createServer((request, response) => {
  response.setHeader("content-type", "application/json");
  if (request.url?.startsWith("/api/v2/alerts")) {
    response.end(
      JSON.stringify([
        {
          labels: {
            alertname: "FlinkTaskManagerOOM",
            severity: "P1",
            component: "flink",
            cluster: "uhadoop-mock",
          },
          annotations: {
            summary: "TaskManager heap usage above 95% for 5 minutes",
          },
          status: { state: "active" },
        },
      ]),
    );
    return;
  }
  response.end(
    JSON.stringify({
      status: "success",
      data: { resultType: "vector", result: [] },
    }),
  );
});

server.listen(port, "127.0.0.1", () => {
  console.error(`mock-prom: listening on http://127.0.0.1:${port}`);
});
