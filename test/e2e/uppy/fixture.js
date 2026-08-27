import Uppy from '@uppy/core'
import AwsS3 from '@uppy/aws-s3'

const params = new URLSearchParams(window.location.search)
const gateway = params.get('gateway')
const accessKey = params.get('accessKey')
const secretKey = params.get('secretKey')
const bucket = params.get('bucket') || 'images'
const providerId = params.get('providerId') || 'ministack-uppy-e2e'
const certifiedUppyVersion = '6.0.0'
const status = document.querySelector('#status')
const details = document.querySelector('#details')

function assert(condition, message) {
  if (!condition) throw new Error(message)
}

async function helperRequest(path, payload) {
  const response = await fetch(`${gateway}/uppy/s3${path}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-S3LS-Access-Key': accessKey,
      'X-S3LS-Secret-Key': secretKey,
    },
    body: JSON.stringify(payload),
  })
  if (!response.ok) {
    throw new Error(`${path} returned ${response.status}: ${await response.text()}`)
  }
  return response.json()
}

function uppyFor({ multipart }) {
  return new Uppy({ autoProceed: false }).use(AwsS3, {
    shouldUseMultipart: multipart,
    getChunkSize: () => 5 * 1024 * 1024,
    generateObjectKey(file) {
      return file.meta.objectKey
    },
    signRequest(request) {
      return helperRequest('/sign', { ...request, bucket, expiresIn: 300 })
    },
  })
}

async function uploadObject({ name, key, type, data, multipart }) {
  const uppy = uppyFor({ multipart })
  try {
    uppy.addFile({ name, type, data: new Blob([data], { type }), meta: { objectKey: key } })
    const result = await uppy.upload()
    assert(result, `${name}: Uppy returned no upload result`)
    assert(result.failed.length === 0, `${name}: ${result.failed.map((file) => file.error).join(', ')}`)
    assert(result.successful.length === 1, `${name}: expected one successful upload`)
  } finally {
    uppy.destroy()
  }
}

async function downloadObject(key) {
  const response = await fetch(`${gateway}/${bucket}/${key}`, {
    headers: {
      'X-S3LS-Access-Key': accessKey,
      'X-S3LS-Secret-Key': secretKey,
    },
  })
  assert(response.ok, `GET ${key} returned ${response.status}`)
  assert(response.headers.get('X-S3LS-Provider-Account') === providerId, `GET ${key} was not routed through MiniStack`)
  return new Uint8Array(await response.arrayBuffer())
}

function equalBytes(actual, expected, label) {
  assert(actual.length === expected.length, `${label}: received ${actual.length} bytes, expected ${expected.length}`)
  for (let index = 0; index < expected.length; index += 1) {
    if (actual[index] !== expected[index]) {
      throw new Error(`${label}: byte ${index} was ${actual[index]}, expected ${expected[index]}`)
    }
  }
}

async function run() {
  assert(gateway && accessKey && secretKey, 'gateway and credentials are required')
  assert(Uppy.VERSION === certifiedUppyVersion, `@uppy/core version ${Uppy.VERSION} is not certified`)
  assert(AwsS3.VERSION === certifiedUppyVersion, `@uppy/aws-s3 version ${AwsS3.VERSION} is not certified`)
  const prefix = `uppy-v6/${crypto.randomUUID()}`

  const direct = new TextEncoder().encode('BucketMux direct upload through Uppy 6')
  const directKey = `${prefix}/direct.txt`
  await uploadObject({ name: 'direct.txt', key: directKey, type: 'text/plain', data: direct, multipart: false })
  equalBytes(await downloadObject(directKey), direct, 'direct upload')

  const multipart = new Uint8Array(6 * 1024 * 1024 + 123)
  for (let index = 0; index < multipart.length; index += 1) multipart[index] = index % 251
  const multipartKey = `${prefix}/multipart.bin`
  await uploadObject({ name: 'multipart.bin', key: multipartKey, type: 'application/octet-stream', data: multipart, multipart: true })
  equalBytes(await downloadObject(multipartKey), multipart, 'multipart upload')

  document.body.dataset.status = 'passed'
  status.textContent = 'passed'
  details.textContent = JSON.stringify({ uppyCore: Uppy.VERSION, uppyAwsS3: AwsS3.VERSION, directKey, multipartKey, multipartBytes: multipart.length }, null, 2)
}

run().catch((error) => {
  console.error(error)
  document.body.dataset.status = 'failed'
  status.textContent = 'failed'
  details.textContent = error?.stack || String(error)
})
