// Steady load for memory/CPU sampling; scripts/run_all.sh samples the gateway's
// cgroup during and after this run to compute CPU per 1k requests and leak drift.
import { check } from 'k6';
import { chat, okJSON, summary, TREND_STATS } from '../lib.js';

const params = {
  rate: Number(__ENV.RATE || 200),
  duration: __ENV.DURATION || '5m',
  latency_ms: Number(__ENV.LATENCY_MS || 10),
};

export const options = {
  summaryTrendStats: TREND_STATS,
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: params.rate,
      timeUnit: '1s',
      duration: params.duration,
      preAllocatedVUs: 50,
      maxVUs: 500,
    },
  },
  thresholds: { checks: ['rate>0.99'] },
};

export default function () {
  const res = chat({ latency_ms: params.latency_ms }, false, '10s');
  check(res, { 'ok completion': okJSON });
}

export function handleSummary(data) {
  return summary(data, params);
}
