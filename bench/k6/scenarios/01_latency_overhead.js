// Added latency per request at a fixed, unsaturated arrival rate. Comparing
// each gateway against the direct-to-mock baseline isolates gateway overhead.
import { check } from 'k6';
import { chat, okJSON, summary, TREND_STATS } from '../lib.js';

const params = {
  rate: Number(__ENV.RATE || 200),
  duration: __ENV.DURATION || '60s',
  latency_ms: Number(__ENV.LATENCY_MS || 0),
};

export const options = {
  summaryTrendStats: TREND_STATS,
  discardResponseBodies: false,
  scenarios: {
    latency: {
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
