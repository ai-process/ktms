// Shared helpers: every target receives byte-identical request bodies, so the
// only difference between runs is the gateway in the path.
import http from 'k6/http';

export const BASE_URL = __ENV.BASE_URL || 'http://ktms:8080';
export const API_KEY = __ENV.API_KEY || 'ktms_bench_key';
export const TARGET = __ENV.TARGET || 'unknown';
export const SUMMARY_PATH = __ENV.SUMMARY_PATH || '/results/summary.json';

export const TREND_STATS = ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'];

// directive is parsed by mock-upstream, e.g. {latency_ms: 0, chunks: 20}.
export function chat(directive, stream, timeout) {
  const tags = Object.entries(directive).map(([k, v]) => `${k}=${v}`).join(' ');
  const body = {
    model: 'gpt-4o-mini',
    messages: [{ role: 'user', content: `Benchmark request [mock ${tags}]` }],
    stream: stream,
  };
  if (stream) {
    body.stream_options = { include_usage: true };
  }
  return http.post(`${BASE_URL}/v1/chat/completions`, JSON.stringify(body), {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${API_KEY}` },
    timeout: timeout || '30s',
  });
}

export function okJSON(res) {
  if (res.status !== 200) return false;
  try {
    const choices = res.json('choices');
    return Array.isArray(choices) && choices.length > 0;
  } catch (e) {
    return false;
  }
}

// summary writes the raw k6 summary plus run parameters for scripts/report.py.
export function summary(data, params) {
  const out = { target: TARGET, params: params, metrics: data.metrics, state: data.state };
  const reqs = data.metrics.http_reqs ? data.metrics.http_reqs.values.count : 0;
  const failed = data.metrics.http_req_failed ? data.metrics.http_req_failed.values.rate : 0;
  return {
    [SUMMARY_PATH]: JSON.stringify(out, null, 2),
    stdout: `${TARGET}: ${reqs} requests, ${(failed * 100).toFixed(2)}% failed -> ${SUMMARY_PATH}\n`,
  };
}
