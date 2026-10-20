// SSE streaming under concurrent streams. mock-upstream sends headers with the
// first token, so TTFB (timings.waiting) is TTFT; total duration exposes buffering.
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { chat, summary, TREND_STATS } from '../lib.js';

const params = {
  vus: Number(__ENV.VUS || 50),
  duration: __ENV.DURATION || '60s',
  latency_ms: Number(__ENV.LATENCY_MS || 0),
  chunks: Number(__ENV.CHUNKS || 20),
  chunk_delay_ms: Number(__ENV.CHUNK_DELAY_MS || 15),
};

const ttft = new Trend('ttft', true);
const streamTotal = new Trend('stream_total', true);
const lostChunks = new Counter('lost_chunks');
const streamFailed = new Rate('stream_failed');

export const options = {
  summaryTrendStats: TREND_STATS,
  scenarios: {
    streams: { executor: 'constant-vus', vus: params.vus, duration: params.duration },
  },
  thresholds: { stream_failed: ['rate<0.01'] },
};

export default function () {
  const res = chat(
    { latency_ms: params.latency_ms, chunks: params.chunks, chunk_delay_ms: params.chunk_delay_ms },
    true,
    '30s',
  );
  const content = contentChunks(res.body || '');
  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'event-stream': (r) => (r.headers['Content-Type'] || '').includes('text/event-stream'),
    'all chunks + [DONE]': (r) => content === params.chunks && (r.body || '').includes('[DONE]'),
  });
  if (res.status === 200) {
    ttft.add(res.timings.waiting);
    streamTotal.add(res.timings.duration);
    lostChunks.add(Math.max(0, params.chunks - content));
  }
  streamFailed.add(!ok);
}

// Counts SSE events carrying non-empty delta content, whatever the gateway's JSON formatting.
function contentChunks(body) {
  let n = 0;
  for (const line of body.split('\n')) {
    if (!line.startsWith('data:')) continue;
    const data = line.slice(5).trim();
    if (data === '' || data === '[DONE]') continue;
    try {
      const choices = JSON.parse(data).choices || [];
      if (choices.some((c) => c.delta && typeof c.delta.content === 'string' && c.delta.content !== '')) n++;
    } catch (e) {
      // A malformed event is not a content chunk; the count check reports it.
    }
  }
  return n;
}

export function handleSummary(data) {
  return summary(data, params);
}
