// Stepped arrival rate to find the saturation point. Requests are tagged with
// the step they were sent in; ramps between steps are tagged "ramp" and ignored.
import { check } from 'k6';
import exec from 'k6/execution';
import { chat, okJSON, summary, TREND_STATS } from '../lib.js';

const params = {
  steps: (__ENV.STEPS || '250,500,1000,1500,2000').split(',').map(Number),
  hold_s: Number(__ENV.HOLD_S || 20),
  ramp_s: Number(__ENV.RAMP_S || 5),
  latency_ms: Number(__ENV.LATENCY_MS || 10),
};

const stages = [];
const windows = [];
let t = 0;
for (const rate of params.steps) {
  stages.push({ target: rate, duration: `${params.ramp_s}s` });
  stages.push({ target: rate, duration: `${params.hold_s}s` });
  windows.push({ rate, from: t + params.ramp_s, to: t + params.ramp_s + params.hold_s });
  t += params.ramp_s + params.hold_s;
}

const thresholds = { checks: ['rate>=0'] };
for (const w of windows) {
  // Thresholds on tagged sub-metrics make k6 export them in the summary.
  thresholds[`http_reqs{step:${w.rate}}`] = ['count>=0'];
  thresholds[`http_req_duration{step:${w.rate}}`] = ['max>=0'];
  thresholds[`checks{step:${w.rate}}`] = ['rate>=0'];
}

export const options = {
  summaryTrendStats: TREND_STATS,
  scenarios: {
    saturation: {
      executor: 'ramping-arrival-rate',
      startRate: 0,
      timeUnit: '1s',
      preAllocatedVUs: 200,
      maxVUs: Number(__ENV.MAX_VUS || 2000),
      stages,
    },
  },
  thresholds,
};

export default function () {
  const elapsed = (Date.now() - exec.scenario.startTime) / 1000;
  const w = windows.find((x) => elapsed >= x.from && elapsed < x.to);
  exec.vu.metrics.tags.step = w ? String(w.rate) : 'ramp';
  const res = chat({ latency_ms: params.latency_ms }, false, '5s');
  check(res, { 'ok completion': okJSON });
}

export function handleSummary(data) {
  return summary(data, params);
}
