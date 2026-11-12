// Run from the repository root with node testdata/interoperability/generate.mjs.
// Uses node:crypto and native JSON.stringify, independently of the Go encoder.
import fs from 'node:fs';
import { createHash, createHmac } from 'node:crypto';

const contract = JSON.parse(fs.readFileSync('../agentrail-js/packages/contract/sdk-contract.json', 'utf8'));
fs.writeFileSync('testdata/interoperability/contract.json', JSON.stringify({ traceHandoff: contract.traceHandoff, markers: contract.markers }, null, 2) + '\n');
const traceparent = contract.traceHandoff.fixtures[0].input.traceparent;
const fixtures = [
  { name: 'html-unicode-separators', hashKey: 'synthetic-密钥-🔑', method: 'pOsT', target: '/café/雪/😀?<>&="\\\t\b\f\u2028\u2029', issuedAt: 1793743200000 },
  { name: 'encoded-path-query', hashKey: 'synthetic-second-key', method: 'GET', target: '/base/%2f/%2F?q=1+2&q=%20&next=%23&empty=', issuedAt: 0 },
  { name: 'empty-trailing-query', hashKey: 'synthetic-second-key', method: 'GET', target: '/path?', issuedAt: 9007199254740991 },
  { name: 'unicode-method-folds', hashKey: 'synthetic-second-key', method: 'KEſ', target: '/method', issuedAt: 1793743200000 },
  { name: 'json-controls', hashKey: 'synthetic-second-key', method: "!#$%&'*+.^_`|~0azAZ-", target: '/controls?x=\u0001\u001f\u000b', issuedAt: 1793743200000 },
];
for (const f of fixtures) {
  if (!/^[!#$%&'*+.^_`|~0-9a-z-]{1,64}$/iu.test(f.method)) throw new Error('Fixture method diverges from core');
  f.traceparent = traceparent;
  f.hashKeyId = createHash('sha256').update(f.hashKey).digest('hex').slice(0, 16);
  const payload = 'agentrail.trace-handoff\0' + JSON.stringify([1, f.hashKeyId, f.issuedAt, f.traceparent, f.method, f.target]);
  f.payloadHex = Buffer.from(payload, 'utf8').toString('hex');
  f.token = `ah1.${f.hashKeyId}.${f.issuedAt}.${f.traceparent}.` + createHmac('sha256', f.hashKey).update(payload).digest('hex');
  f.hashValue = 'principal/é/😀';
  f.hash = createHmac('sha256', f.hashKey).update(f.hashValue).digest('hex').slice(0, 32);
}
fs.writeFileSync('testdata/interoperability/javascript.json', JSON.stringify(fixtures, null, 2) + '\n');

// Read actual core marker semantics, including Unicode uppercase expansions.
const { parseMarker } = await import('../../../agentrail-js/packages/core/src/marker.ts');
const alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
const markers = [];
for (const base of ['ß00000000000', 'ﬃ0000000000', 'ı000000000000', 'ſ000000000000']) {
  const raw = [...alphabet].map(c => base + c).find(value => parseMarker(value).valid);
  if (raw === undefined) throw new Error('No marker checksum found');
  markers.push({ raw, valid: true });
  const invalid = raw.slice(0, -1) + (raw.endsWith('0') ? '1' : '0');
  markers.push({ raw: invalid, valid: false });
}
for (const raw of ['K0000000000000', 'İ0000000000000']) markers.push({ raw, valid: parseMarker(raw).valid });
fs.writeFileSync('testdata/interoperability/markers.json', JSON.stringify(markers, null, 2) + '\n');

// Operation references hash exactly as agentrail-node's performance.ts does:
// HMAC-SHA256 over JSON.stringify(['agentrail.operation', namespaceId, instanceRef]).
const operations = [
  { name: 'node-test-vector', hashKey: 'synthetic-workspace-key', namespaceId: '11111111-1111-4111-8111-111111111111', instanceRef: 'synthetic-private-operation-attempt\u0000' },
  { name: 'html-unicode-separators', hashKey: 'synthetic-密钥-🔑', namespaceId: '22222222-2222-4222-8222-222222222222', instanceRef: 'attempt/<>&="\\\t\b\f  /é/😀' },
  { name: 'json-controls', hashKey: 'synthetic-second-key', namespaceId: '33333333-3333-4333-a333-333333333333', instanceRef: '\u0001\u001f\u000b\r\n' },
];
for (const o of operations) {
  o.hash = createHmac('sha256', o.hashKey).update(JSON.stringify(['agentrail.operation', o.namespaceId, o.instanceRef])).digest('hex');
}
fs.writeFileSync('testdata/interoperability/operations.json', JSON.stringify(operations, null, 2) + '\n');
