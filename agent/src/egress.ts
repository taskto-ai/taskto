import dns, { type LookupAddress } from 'node:dns'
import { isIP } from 'node:net'
import { Agent, fetch as undiciFetch } from 'undici'

type Prefix = { bytes: number[]; bits: number }

export type Policy = { public: boolean; hosts: string[]; prefixes: Prefix[] }

export const REFUSED = "the server's egress policy doesn't allow this endpoint"

export class EgressRefused extends Error {}

export function parsePolicy(value: unknown, at: string): Policy {
  if (!Array.isArray(value) || value.length === 0 || value.length > 256) {
    throw new Error(`${at} must be a non-empty array of entries`)
  }
  const policy: Policy = { public: false, hosts: [], prefixes: [] }
  value.forEach((entry, i) => {
    if (typeof entry !== 'string') throw new Error(`${at}[${i}] must be a string`)
    if (entry === 'public') {
      policy.public = true
      return
    }
    if (entry === 'any') throw new Error(`${at}[${i}]: leave ${at} out to allow any address`)
    const [address, bits, extra] = entry.split('/')
    const bytes = address === undefined ? undefined : parseAddr(address)
    if (bytes && extra === undefined) {
      const max = bytes.length * 8
      const n = bits === undefined ? max : /^\d{1,3}$/.test(bits) ? Number(bits) : Number.NaN
      if (!(n >= 0 && n <= max)) throw new Error(`${at}[${i}] isn't a CIDR prefix`)
      policy.prefixes.push(unmapPrefix({ bytes, bits: n }))
      return
    }
    const host = normalizeHost(entry)
    if (!/^[a-z0-9_-]{1,63}(\.[a-z0-9_-]{1,63})*$/.test(host) || host.length > 253) {
      throw new Error(`${at}[${i}] must be public, a host name, an IP address or a CIDR prefix`)
    }
    policy.hosts.push(host)
  })
  return policy
}

function normalizeHost(host: string): string {
  return host.toLowerCase().replace(/\.$/, '')
}

function allowsHost(policy: Policy, host: string): boolean {
  return policy.hosts.includes(normalizeHost(host))
}

export function refusal(policy: Policy, host: string, addr: string): EgressRefused | undefined {
  const parsed = parseAddr(addr)
  if (!parsed) return new EgressRefused(`${REFUSED}: ${host} resolves to an address that can't be checked`)
  const bytes = unmap(parsed)
  if (policy.prefixes.some((p) => contains(p, bytes))) return undefined
  let reason = 'not on the allowlist'
  if (policy.public) {
    const kind = special(bytes)
    if (kind === '') return undefined
    reason = `a ${kind} address`
  }
  const shown = bytes.length === parsed.length ? addr.replace(/%.*$/, '') : bytes.join('.')
  return new EgressRefused(
    host === shown || host === addr
      ? `${REFUSED}: ${host} is ${reason}`
      : `${REFUSED}: ${host} resolves to ${shown}, ${reason}`,
  )
}

const SPECIAL: [string, string][] = [
  ['0.0.0.0/8', 'reserved'],
  ['10.0.0.0/8', 'private'],
  ['100.64.0.0/10', 'CGNAT'],
  ['127.0.0.0/8', 'loopback'],
  ['169.254.0.0/16', 'link-local'],
  ['168.63.129.16/32', 'cloud metadata'],
  ['172.16.0.0/12', 'private'],
  ['192.0.0.0/24', 'reserved'],
  ['192.0.2.0/24', 'documentation'],
  ['192.88.99.0/24', 'reserved'],
  ['192.168.0.0/16', 'private'],
  ['198.18.0.0/15', 'reserved'],
  ['198.51.100.0/24', 'documentation'],
  ['203.0.113.0/24', 'documentation'],
  ['224.0.0.0/4', 'multicast'],
  ['240.0.0.0/4', 'reserved'],
  ['::/128', 'reserved'],
  ['::1/128', 'loopback'],
  ['64:ff9b:1::/48', 'private'],
  ['100::/64', 'reserved'],
  ['2001::/23', 'reserved'],
  ['2001:db8::/32', 'documentation'],
  ['3fff::/20', 'documentation'],
  ['fc00::/7', 'unique-local'],
  ['fe80::/10', 'link-local'],
  ['ff00::/8', 'multicast'],
]
const specialRanges = SPECIAL.map(([cidr, kind]) => {
  const [address, bits] = cidr.split('/') as [string, string]
  return { prefix: { bytes: parseAddr(address) as number[], bits: Number(bits) }, kind }
})
const NAT64 = { bytes: parseAddr('64:ff9b::') as number[], bits: 96 }
const SIX_TO_FOUR = { bytes: parseAddr('2002::') as number[], bits: 16 }
const GLOBAL6 = { bytes: parseAddr('2000::') as number[], bits: 3 }

export function special(bytes: number[]): string {
  bytes = unmap(bytes)
  if (bytes.length === 16) {
    if (contains(NAT64, bytes)) return special(bytes.slice(12))
    if (contains(SIX_TO_FOUR, bytes)) return special(bytes.slice(2, 6))
  }
  for (const r of specialRanges) {
    if (contains(r.prefix, bytes)) return r.kind
  }
  return bytes.length === 16 && !contains(GLOBAL6, bytes) ? 'reserved' : ''
}

function contains(prefix: Prefix, bytes: number[]): boolean {
  if (prefix.bytes.length !== bytes.length) return false
  for (let bit = 0; bit < prefix.bits; bit++) {
    const mask = 0x80 >> (bit % 8)
    if (((prefix.bytes[bit >> 3] as number) & mask) !== ((bytes[bit >> 3] as number) & mask)) return false
  }
  return true
}

const MAPPED = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff]

function unmap(bytes: number[]): number[] {
  return bytes.length === 16 && MAPPED.every((b, i) => bytes[i] === b) ? bytes.slice(12) : bytes
}

function unmapPrefix(p: Prefix): Prefix {
  return p.bytes.length === 16 && p.bits >= 96 && unmap(p.bytes).length === 4
    ? { bytes: p.bytes.slice(12), bits: p.bits - 96 }
    : p
}

function parseAddr(text: string): number[] | undefined {
  text = text.replace(/^\[(.*)\]$/, '$1').replace(/%.*$/, '')
  if (isIP(text) === 4) return text.split('.').map(Number)
  if (isIP(text) !== 6) return undefined
  let tail: number[] = []
  const v4 = text.match(/:(\d+\.\d+\.\d+\.\d+)$/)
  if (v4?.[1]) {
    tail = v4[1].split('.').map(Number)
    text = `${text.slice(0, -v4[1].length)}0:0`
  }
  const [head = '', rest] = text.split('::')
  const groups = (s: string) => (s === '' ? [] : s.split(':').map((g) => Number.parseInt(g, 16)))
  const left = groups(head)
  const right = rest === undefined ? [] : groups(rest)
  const words = [...left, ...Array(8 - left.length - right.length).fill(0), ...right]
  const bytes = words.flatMap((w) => [w >> 8, w & 0xff])
  if (tail.length) bytes.splice(12, 4, ...tail)
  return bytes
}

type LookupCallback = (err: NodeJS.ErrnoException | null, address: string | LookupAddress[], family?: number) => void

function lookupFor(policy: Policy) {
  return (hostname: string, options: dns.LookupOptions, callback: LookupCallback): void => {
    dns.lookup(hostname, { ...options, all: true }, (err, addresses) => {
      if (err) return callback(err, [])
      let allowed = addresses
      if (!allowsHost(policy, hostname)) {
        const refused = addresses.map((a) => refusal(policy, hostname, a.address))
        allowed = addresses.filter((_, i) => refused[i] === undefined)
        const first = refused.find((r) => r !== undefined)
        if (allowed.length === 0 && first) return callback(first, [])
      }
      if (options.all) return callback(null, allowed)
      const [one] = allowed
      if (!one) return callback(Object.assign(new Error(`no addresses for ${hostname}`), { code: 'ENOTFOUND' }), [])
      callback(null, one.address, one.family)
    })
  }
}

type UndiciInput = Parameters<typeof undiciFetch>[0]
type UndiciInit = NonNullable<Parameters<typeof undiciFetch>[1]>

export function egressFetch(policy: Policy): (onRefused: (refused: EgressRefused) => void) => typeof globalThis.fetch {
  const dispatcher = new Agent({ connect: { lookup: lookupFor(policy) } })
  return (onRefused) => {
    const guarded = async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(input instanceof Request ? input.url : input)
      const host = url.hostname.replace(/^\[(.*)\]$/, '$1')
      if (isIP(host) && !allowsHost(policy, host)) {
        const refused = refusal(policy, host, host)
        if (refused) {
          onRefused(refused)
          throw refused
        }
      }
      try {
        return await undiciFetch(input as unknown as UndiciInput, {
          ...(init as unknown as UndiciInit),
          dispatcher,
          redirect: 'manual',
        })
      } catch (error) {
        for (let e: unknown = error; e instanceof Error; e = e.cause) {
          if (e instanceof EgressRefused) onRefused(e)
        }
        throw error
      }
    }
    return guarded as unknown as typeof globalThis.fetch
  }
}
