import http from 'k6/http'
import exec from 'k6/execution'
import { check } from 'k6'
import { Counter, Rate, Trend } from 'k6/metrics'

const baseURL = __ENV.BASE_URL || 'http://127.0.0.1:18089'
const accessKey = __ENV.S3_ACCESS_KEY || 'k6-access-key'
const secretKey = __ENV.S3_SECRET_KEY || 'k6-secret-key'
const seedCount = Number(__ENV.BUCKETMUX_K6_SEED_OBJECTS || 100)
const payloadBytes = Number(__ENV.BUCKETMUX_K6_PAYLOAD_BYTES || 4096)
const payload = 'x'.repeat(payloadBytes)

const requestErrors = new Rate('s3_request_errors')
const completedOperations = new Counter('s3_completed_operations')
const getDuration = new Trend('s3_get_duration', true)
const headDuration = new Trend('s3_head_duration', true)
const listDuration = new Trend('s3_list_duration', true)
const putDuration = new Trend('s3_put_duration', true)
const deleteDuration = new Trend('s3_delete_duration', true)

export const options = {
  scenarios: {
    mixed_s3_workload: {
      executor: 'constant-vus',
      vus: Number(__ENV.BUCKETMUX_K6_VUS || 20),
      duration: __ENV.BUCKETMUX_K6_DURATION || '30s',
      gracefulStop: '5s',
    },
  },
  thresholds: {
    s3_request_errors: ['rate<0.01'],
    s3_get_duration: ['p(95)<50', 'p(99)<100'],
    s3_head_duration: ['p(95)<50', 'p(99)<100'],
    s3_list_duration: ['p(95)<100', 'p(99)<200'],
    s3_put_duration: ['p(95)<100', 'p(99)<200'],
    s3_delete_duration: ['p(95)<100', 'p(99)<200'],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  noConnectionReuse: false,
  userAgent: 'BucketMux-k6-performance/1.0',
}

function params(operation, contentType = '', metricName = operation) {
  const headers = {
    'X-S3LS-Access-Key': accessKey,
    'X-S3LS-Secret-Key': secretKey,
  }
  if (contentType) headers['Content-Type'] = contentType
  return { headers, tags: { operation, name: metricName } }
}

function record(response, expectedStatus, trend, operation, validateBody = false) {
  trend.add(response.timings.duration)
  const ok = check(response, {
    [`${operation} status ${expectedStatus}`]: (res) => res.status === expectedStatus,
    [`${operation} has no server error`]: (res) => res.status < 500,
    ...(validateBody ? { [`${operation} body size`]: (res) => res.body.length === payloadBytes } : {}),
  })
  requestErrors.add(!ok)
  if (ok) completedOperations.add(1, { operation })
  return ok
}

export function setup() {
  for (let index = 0; index < seedCount; index += 1) {
    const key = `seed/object-${String(index).padStart(4, '0')}.bin`
    const response = http.put(`${baseURL}/performance/${key}`, payload, params('seed', 'application/octet-stream', 'PUT /performance/seed/:key'))
    if (response.status !== 200) {
      throw new Error(`seed PUT ${key} failed with status ${response.status}: ${response.body}`)
    }
  }
  return { seedCount }
}

export default function (data) {
  const selector = exec.scenario.iterationInTest % 10
  const seedIndex = exec.scenario.iterationInTest % data.seedCount
  const seedKey = `seed/object-${String(seedIndex).padStart(4, '0')}.bin`
  const seedURL = `${baseURL}/performance/${seedKey}`

  if (selector < 5) {
    const response = http.get(seedURL, params('get', '', 'GET /performance/:key'))
    record(response, 200, getDuration, 'GET', true)
    return
  }
  if (selector < 8) {
    const response = http.head(seedURL, params('head', '', 'HEAD /performance/:key'))
    record(response, 200, headDuration, 'HEAD')
    return
  }
  if (selector === 8) {
    const response = http.get(`${baseURL}/performance?list-type=2&prefix=seed/&max-keys=100`, params('list', '', 'GET /performance?list-type=2'))
    record(response, 200, listDuration, 'LIST')
    return
  }

  const dynamicKey = `dynamic/vu-${__VU}/iteration-${__ITER}.bin`
  const dynamicURL = `${baseURL}/performance/${dynamicKey}`
  const putResponse = http.put(dynamicURL, payload, params('put', 'application/octet-stream', 'PUT /performance/:key'))
  if (!record(putResponse, 200, putDuration, 'PUT')) return
  const getResponse = http.get(dynamicURL, params('get-after-put', '', 'GET /performance/:key'))
  if (!record(getResponse, 200, getDuration, 'GET after PUT', true)) return
  const deleteResponse = http.del(dynamicURL, null, params('delete', '', 'DELETE /performance/:key'))
  record(deleteResponse, 204, deleteDuration, 'DELETE')
}
