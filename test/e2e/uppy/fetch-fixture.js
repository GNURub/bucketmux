const params = new URLSearchParams(window.location.search)
const gateway = params.get('gateway')
const accessKey = params.get('accessKey')
const secretKey = params.get('secretKey')
const bucket = params.get('bucket') || 'images'
const providerId = params.get('providerId') || 'ministack-fetch-e2e'
const status = document.querySelector('#status')
const details = document.querySelector('#details')

function assert(condition, message) {
  if (!condition) throw new Error(message)
}

async function sign(request) {
  const response = await fetch(`${gateway}/uppy/s3/sign`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-S3LS-Access-Key': accessKey,
      'X-S3LS-Secret-Key': secretKey,
    },
    body: JSON.stringify({ ...request, bucket, expiresIn: 300 }),
  })
  if (!response.ok) throw new Error(`sign ${request.method} returned ${response.status}: ${await response.text()}`)
  return response.json()
}

async function presignedFetch(request, init = {}) {
  const { url } = await sign(request)
  const response = await fetch(url, { ...init, method: request.method })
  if (!response.ok) throw new Error(`${request.method} ${request.key} returned ${response.status}: ${await response.text()}`)
  return response
}

function xmlText(xml, name) {
  const document = new DOMParser().parseFromString(xml, 'application/xml')
  const parserError = document.querySelector('parsererror')
  if (parserError) throw new Error(`invalid XML response: ${parserError.textContent}`)
  return document.getElementsByTagName(name)[0]?.textContent || ''
}

function escapeXML(value) {
  return value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
}

function equalBytes(actual, expected) {
  assert(actual.length === expected.length, `received ${actual.length} bytes, expected ${expected.length}`)
  for (let index = 0; index < expected.length; index += 1) {
    if (actual[index] !== expected[index]) throw new Error(`byte ${index} was ${actual[index]}, expected ${expected[index]}`)
  }
}

async function run() {
  assert(gateway && accessKey && secretKey, 'gateway and credentials are required')
  const key = `fetch-presigned/${crypto.randomUUID()}/multipart.bin`
  const payload = new Uint8Array(6 * 1024 * 1024 + 321)
  for (let index = 0; index < payload.length; index += 1) payload[index] = (index * 31) % 251

  const created = await presignedFetch({ method: 'POST', key }, {
    headers: { 'Content-Type': 'application/octet-stream' },
  })
  const uploadId = xmlText(await created.text(), 'UploadId')
  assert(uploadId, 'multipart creation returned no UploadId')

  const chunkSize = 5 * 1024 * 1024
  const completedParts = []
  for (let offset = 0, partNumber = 1; offset < payload.length; offset += chunkSize, partNumber += 1) {
    const body = payload.slice(offset, Math.min(offset + chunkSize, payload.length))
    const uploaded = await presignedFetch({ method: 'PUT', key, uploadId, partNumber }, { body })
    const etag = uploaded.headers.get('ETag')
    assert(etag, `part ${partNumber} response did not expose ETag`)
    completedParts.push({ partNumber, etag, size: body.length })
  }

  const listed = await presignedFetch({ method: 'GET', key, uploadId })
  const listedDocument = new DOMParser().parseFromString(await listed.text(), 'application/xml')
  const listedParts = [...listedDocument.getElementsByTagName('Part')]
  assert(listedParts.length === completedParts.length, `listed ${listedParts.length} parts, expected ${completedParts.length}`)
  completedParts.forEach((part, index) => {
    assert(Number(xmlText(listedParts[index].outerHTML, 'PartNumber')) === part.partNumber, `listed part ${index + 1} has wrong number`)
    assert(Number(xmlText(listedParts[index].outerHTML, 'Size')) === part.size, `listed part ${part.partNumber} has wrong size`)
  })

  const completeBody = `<CompleteMultipartUpload>${completedParts.map((part) => `<Part><PartNumber>${part.partNumber}</PartNumber><ETag>${escapeXML(part.etag)}</ETag></Part>`).join('')}</CompleteMultipartUpload>`
  const completed = await presignedFetch({ method: 'POST', key, uploadId }, {
    headers: { 'Content-Type': 'application/xml' },
    body: completeBody,
  })
  assert(xmlText(await completed.text(), 'Key') === key, 'multipart completion returned the wrong key')

  const downloaded = await presignedFetch({ method: 'GET', key })
  assert(downloaded.headers.get('X-S3LS-Provider-Account') === providerId, 'download was not routed through MiniStack')
  equalBytes(new Uint8Array(await downloaded.arrayBuffer()), payload)

  const deleted = await presignedFetch({ method: 'DELETE', key })
  assert(deleted.status === 204, `DELETE returned ${deleted.status}`)
  const { url: deletedObjectURL } = await sign({ method: 'GET', key })
  const missing = await fetch(deletedObjectURL)
  assert(missing.status === 404, `GET after DELETE returned ${missing.status}`)

  document.body.dataset.status = 'passed'
  status.textContent = 'passed'
  details.textContent = JSON.stringify({ api: 'window.fetch', key, uploadId, parts: completedParts.length, bytes: payload.length, allDataRequestsPresigned: true }, null, 2)
}

run().catch((error) => {
  console.error(error)
  document.body.dataset.status = 'failed'
  status.textContent = 'failed'
  details.textContent = error?.stack || String(error)
})
