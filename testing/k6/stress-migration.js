import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";
import exec from "k6/execution";

const baseUrl = (__ENV.BASE_URL || "http://localhost:6969").replace(/\/$/, "");
const username = __ENV.K6_USERNAME || "admin";
const password = __ENV.K6_PASSWORD || "admin1234";
const normalVus = Number(__ENV.NORMAL_VUS || 5);
const databaseIds = (__ENV.DATABASE_IDS || "1").split(",").map((value) => value.trim()).filter(Boolean);
const migrationVersions = (__ENV.MIGRATION_VERSIONS || "1").split(",").map((value) => value.trim()).filter(Boolean);

const apiDuration = new Trend("api_duration_ms", true);
const queueWait = new Trend("migration_queue_wait_ms", true);
const jobDuration = new Trend("migration_job_duration_ms", true);
const apiErrors = new Rate("api_error_rate");
const jobFailures = new Rate("migration_job_failure_rate");
const successfulJobs = new Counter("migration_jobs_success");
const submittedJobs = new Counter("migration_jobs_submitted");

export const options = {
  scenarios: {
    combined: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: __ENV.RAMP_UP || "1m", target: normalVus },
        { duration: __ENV.NORMAL_DURATION || "2m", target: normalVus },
        { duration: __ENV.STRESS_DURATION || "2m", target: normalVus * 2 },
        { duration: __ENV.PEAK_DURATION || "2m", target: normalVus * 4 },
        { duration: __ENV.COOLDOWN_DURATION || "2m", target: normalVus },
        { duration: __ENV.RECOVERY_DURATION || "1m", target: 0 },
      ],
    },
  },
  thresholds: {
    api_duration_ms: [`p(95)<${__ENV.API_P95_THRESHOLD_MS || 500}`],
    api_error_rate: [`rate<${__ENV.API_ERROR_THRESHOLD || 0.05}`],
  },
};

let loggedIn = false;

function getCsrfToken() {
  const cookies = http.cookieJar().cookiesForURL(baseUrl);
  const values = cookies.csrf_token;
  return values && values.length ? values[0] : "";
}

function login() {
  const loginResponse = http.get(`${baseUrl}/login`, { tags: { request_type: "auth" } });
  const csrf = getCsrfToken();
  const response = http.post(`${baseUrl}/login`, {
    username,
    password,
    _csrf: csrf,
  }, {
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    redirects: 5,
    tags: { request_type: "auth" },
  });
  const whoami = http.get(`${baseUrl}/whoami`, { tags: { request_type: "auth" } });
  loggedIn = response.status < 400 && whoami.status === 200;
  check(whoami, { "login succeeds": (r) => r.status === 200 });
}

function csrfHeaders() {
  return {
    "Content-Type": "application/json",
    "X-CSRF-Token": getCsrfToken(),
  };
}

function apiGet(path, name) {
  let response = http.get(`${baseUrl}${path}`, { tags: { request_type: "api", endpoint: name } });
  if (response.status === 401) {
    login();
    response = http.get(`${baseUrl}${path}`, { tags: { request_type: "api", endpoint: name } });
  }
  apiDuration.add(response.timings.duration);
  const ok = response.status >= 200 && response.status < 300;
  apiErrors.add(!ok);
  check(response, { [`${name} returns success`]: () => ok });
  return response;
}

function chooseMigration() {
  const index = exec.scenario.iterationInTest % databaseIds.length;
  return {
    index,
    databaseId: databaseIds[index],
    version: migrationVersions[index] || migrationVersions[0],
  };
}

function pollJob(databaseId, jobId, submittedAt) {
  const deadline = Date.now() + Number(__ENV.JOB_TIMEOUT_MS || 120000);
  let runningAt = null;
  while (Date.now() < deadline) {
    const response = apiGet(`/api/databases/${databaseId}/jobs/${jobId}`, "job_status");
    if (response.status !== 200) {
      sleep(0.25);
      continue;
    }
    const job = response.json();
    if (job.status !== "pending_review" && job.status !== "pending" && runningAt === null) {
      runningAt = Date.now();
      queueWait.add(runningAt - submittedAt);
    }
    if (job.status === "success") {
      jobDuration.add(Date.now() - submittedAt);
      successfulJobs.add(1);
      jobFailures.add(false);
      return;
    }
    if (["failed", "cancelled"].includes(job.status)) {
      jobDuration.add(Date.now() - submittedAt);
      jobFailures.add(true);
      return;
    }
    sleep(0.25);
  }
  jobDuration.add(Date.now() - submittedAt);
  jobFailures.add(true);
}

export default function () {
  if (!loggedIn) login();
  if (!loggedIn) {
    sleep(1);
    return;
  }

  const migration = chooseMigration();
  apiGet(`/api/databases/${migration.databaseId}/migrations`, "list_migrations");
  apiGet(`/api/databases/${migration.databaseId}/jobs`, "list_jobs");

  if (migration.index >= databaseIds.length || exec.scenario.iterationInTest >= databaseIds.length) {
    sleep(0.5);
    return;
  }

  const submittedAt = Date.now();
  let response = http.post(
    `${baseUrl}/api/databases/${migration.databaseId}/migrations/${migration.version}/execute`,
    JSON.stringify({ mode: "normal", batchTable: "", batchSize: 0 }),
    {
      headers: csrfHeaders(),
      tags: { request_type: "job_submit", endpoint: "execute_migration" },
    },
  );
  if (response.status === 401) {
    login();
    response = http.post(
      `${baseUrl}/api/databases/${migration.databaseId}/migrations/${migration.version}/execute`,
      JSON.stringify({ mode: "normal", batchTable: "", batchSize: 0 }),
      {
        headers: csrfHeaders(),
        tags: { request_type: "job_submit", endpoint: "execute_migration" },
      },
    );
  }
  apiDuration.add(response.timings.duration);
  const submitted = response.status === 202;
  apiErrors.add(!submitted);
  check(response, { "job submission accepted": () => submitted });
  if (submitted) {
    submittedJobs.add(1);
    const job = response.json();
    pollJob(migration.databaseId, job.jobId, submittedAt);
  }
  sleep(0.5);
}

export function handleSummary(data) {
  return {
    "testing/k6/stress-migration-summary.json": JSON.stringify(data, null, 2),
    stdout: JSON.stringify({
      api_p50_ms: data.metrics.api_duration_ms?.values?.med ?? null,
      api_p95_ms: data.metrics.api_duration_ms?.values?.["p(95)"] ?? null,
      http_request_throughput: data.metrics.http_reqs?.values?.rate ?? null,
      http_error_rate: data.metrics.http_req_failed?.values?.rate ?? null,
      submitted_jobs: data.metrics.migration_jobs_submitted?.values?.count ?? 0,
      completed_jobs: data.metrics.migration_jobs_success?.values?.count ?? 0,
      job_completion_throughput: data.metrics.migration_jobs_success?.values?.rate ?? null,
      queue_wait_p95_ms: data.metrics.migration_queue_wait_ms?.values?.["p(95)"] ?? null,
      job_duration_p95_ms: data.metrics.migration_job_duration_ms?.values?.["p(95)"] ?? null,
      job_failure_rate: data.metrics.migration_job_failure_rate?.values?.rate ?? null,
    }, null, 2),
  };
}
