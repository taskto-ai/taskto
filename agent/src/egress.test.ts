import dns from 'node:dns'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { EgressRefused, egressFetch, parsePolicy, REFUSED, refusal, special } from './egress.js'

const bytes = (address: string) => address.split('.').map(Number)

describe('parsePolicy', () => {
  it('reads public, hosts, addresses and prefixes', () => {
    const policy = parsePolicy(
      ['public', 'Models.Example.com.', '10.1.2.3', '192.168.0.0/16', '2001:db8::/32'],
      'egress',
    )
    expect(policy.public).toBe(true)
    expect(policy.hosts).toEqual(['models.example.com'])
    expect(policy.prefixes).toHaveLength(3)
  })

  it('takes an IPv4-mapped prefix as the IPv4 one', () => {
    const policy = parsePolicy(['::ffff:10.0.0.0/104'], 'egress')
    expect(policy.prefixes).toEqual([{ bytes: [10, 0, 0, 0], bits: 8 }])
  })

  it.each([
    [[], 'egress must be a non-empty array of entries'],
    ['public', 'egress must be a non-empty array of entries'],
    [[1], 'egress[0] must be a string'],
    [['any'], 'egress[0]: leave egress out to allow any address'],
    [['10.0.0.0/33'], "egress[0] isn't a CIDR prefix"],
    [['10.0.0.0/x'], "egress[0] isn't a CIDR prefix"],
    [['::/129'], "egress[0] isn't a CIDR prefix"],
    [['bad host'], 'egress[0] must be public, a host name, an IP address or a CIDR prefix'],
    [['http://example.com'], 'egress[0] must be public, a host name, an IP address or a CIDR prefix'],
    [['10.0.0.0/8/8'], 'egress[0] must be public, a host name, an IP address or a CIDR prefix'],
  ])('refuses %j', (value, message) => {
    expect(() => parsePolicy(value, 'egress')).toThrow(message)
  })

  it('refuses a list that is too long, and never quotes a value', () => {
    expect(() =>
      parsePolicy(
        Array.from({ length: 257 }, () => 'public'),
        'egress',
      ),
    ).toThrow('non-empty array')
    expect(() => parsePolicy(['not a host: hunter2'], 'egress')).not.toThrow(/hunter2/)
  })
})

describe('special', () => {
  it.each([
    ['10.0.0.1', 'private'],
    ['10.255.255.254', 'private'],
    ['172.31.255.254', 'private'],
    ['192.168.255.254', 'private'],
    ['127.255.255.254', 'loopback'],
    ['172.16.5.5', 'private'],
    ['192.168.1.1', 'private'],
    ['127.0.0.1', 'loopback'],
    ['169.254.169.254', 'link-local'],
    ['168.63.129.16', 'cloud metadata'],
    ['100.64.0.1', 'CGNAT'],
    ['224.0.0.1', 'multicast'],
    ['0.0.0.0', 'reserved'],
    ['203.0.113.7', 'documentation'],
  ])('%s is %s', (address, kind) => {
    expect(special(bytes(address))).toBe(kind)
  })

  it.each(['8.8.8.8', '1.1.1.1', '172.32.0.1', '100.128.0.1'])('%s is public', (address) => {
    expect(special(bytes(address))).toBe('')
  })

  it('looks through the IPv6 forms that carry an IPv4 address', () => {
    const v6 = (text: string) => {
      const policy = parsePolicy([`${text}/128`], 'egress')
      return policy.prefixes[0]?.bytes as number[]
    }
    // IPv4-mapped
    expect(special(v6('::ffff:10.0.0.1'))).toBe('private')
    expect(special(v6('::ffff:8.8.8.8'))).toBe('')
    // NAT64 of a private and of a public address
    expect(special(v6('64:ff9b::a00:1'))).toBe('private')
    expect(special(v6('64:ff9b::808:808'))).toBe('')
    // 6to4 of 10.0.0.1 (2002:0a00:0001::)
    expect(special(v6('2002:a00:1::'))).toBe('private')
  })

  it('knows IPv6 that is not public', () => {
    const v6 = (text: string) => parsePolicy([`${text}/128`], 'egress').prefixes[0]?.bytes as number[]
    expect(special(v6('::1'))).toBe('loopback')
    expect(special(v6('fe80::1'))).toBe('link-local')
    expect(special(v6('fd00::1'))).toBe('unique-local')
    expect(special(v6('2001:db8::1'))).toBe('documentation')
    expect(special(v6('2606:4700:4700::1111'))).toBe('')
    // Outside 2000::/3, the global unicast range
    expect(special(v6('4000::1'))).toBe('reserved')
  })
})

describe('refusal', () => {
  it('allows what "public" allows, and refuses the rest naming why', () => {
    const policy = parsePolicy(['public'], 'egress')
    expect(refusal(policy, 'api.example.com', '8.8.8.8')).toBeUndefined()
    const refused = refusal(policy, 'api.example.com', '10.0.0.5')
    expect(refused).toBeInstanceOf(EgressRefused)
    expect(refused?.message).toBe(`${REFUSED}: api.example.com resolves to 10.0.0.5, a private address`)
  })

  it('says it plainly when the host is the address itself', () => {
    const policy = parsePolicy(['public'], 'egress')
    expect(refusal(policy, '127.0.0.1', '127.0.0.1')?.message).toBe(`${REFUSED}: 127.0.0.1 is a loopback address`)
  })

  it('allows an address a prefix lists, even a non-public one', () => {
    const policy = parsePolicy(['public', '10.1.0.0/16'], 'egress')
    expect(refusal(policy, 'gateway', '10.1.2.3')).toBeUndefined()
    expect(refusal(policy, 'gateway', '10.2.0.1')).toBeInstanceOf(EgressRefused)
  })

  it('refuses everything off an allowlist that has no "public"', () => {
    const policy = parsePolicy(['203.0.113.0/24'], 'egress')
    expect(refusal(policy, 'a.example', '203.0.113.9')).toBeUndefined()
    expect(refusal(policy, 'a.example', '8.8.8.8')?.message).toBe(
      `${REFUSED}: a.example resolves to 8.8.8.8, not on the allowlist`,
    )
  })

  it('matches an IPv4 prefix against an IPv4-mapped address, and shows it as IPv4', () => {
    const policy = parsePolicy(['10.0.0.0/8'], 'egress')
    expect(refusal(policy, 'h', '::ffff:10.9.9.9')).toBeUndefined()
    expect(refusal(policy, 'h', '::ffff:192.168.0.1')?.message).toContain('resolves to 192.168.0.1')
  })

  it('refuses an address it cannot parse', () => {
    const policy = parsePolicy(['public'], 'egress')
    expect(refusal(policy, 'h', 'not-an-address')?.message).toBe(
      `${REFUSED}: h resolves to an address that can't be checked`,
    )
  })

  it('drops an IPv6 zone', () => {
    const policy = parsePolicy(['public'], 'egress')
    expect(refusal(policy, 'h', 'fe80::1%eth0')?.message).toContain('resolves to fe80::1, a link-local address')
  })
})

describe('egressFetch', () => {
  let server: Server
  let port: number
  let reached: string[] = []

  afterEach(() => {
    vi.restoreAllMocks()
    reached = []
  })

  beforeAll(async () => {
    server = createServer((req, res) => {
      reached.push(req.url ?? '')
      if (req.url === '/redirect') {
        res.writeHead(302, { Location: 'http://127.0.0.1:1/elsewhere' }).end()
        return
      }
      // Close the connection after each answer, so the next request has to connect, and look the name up, again.
      res.writeHead(200, { 'Content-Type': 'text/plain', Connection: 'close' }).end('hello')
    })
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
    port = (server.address() as AddressInfo).port
  })

  afterAll(async () => {
    server.closeAllConnections()
    await new Promise((resolve) => server.close(resolve))
  })

  it('refuses a literal address the policy does not allow, before connecting, and tells onRefused', async () => {
    const heard: EgressRefused[] = []
    const fetcher = egressFetch(parsePolicy(['public'], 'egress'))((r) => heard.push(r))
    await expect(fetcher(`http://127.0.0.1:${port}/`)).rejects.toBeInstanceOf(EgressRefused)
    expect(heard).toHaveLength(1)
    expect(heard[0]?.message).toBe(`${REFUSED}: 127.0.0.1 is a loopback address`)
  })

  it('checks a bracketed IPv6 literal too', async () => {
    const heard: EgressRefused[] = []
    const fetcher = egressFetch(parsePolicy(['public'], 'egress'))((r) => heard.push(r))
    await expect(fetcher('http://[::1]:9/')).rejects.toBeInstanceOf(EgressRefused)
    expect(heard[0]?.message).toBe(`${REFUSED}: ::1 is a loopback address`)
  })

  it('refuses a name that resolves to an address the policy does not allow', async () => {
    const heard: EgressRefused[] = []
    const fetcher = egressFetch(parsePolicy(['public'], 'egress'))((r) => heard.push(r))
    await expect(fetcher(`http://localhost:${port}/`)).rejects.toThrow()
    expect(heard).toHaveLength(1)
    expect(heard[0]?.message).toMatch(/^the server's egress policy doesn't allow this endpoint: localhost resolves to /)
  })

  it('looks a name up for every connection, so a name that later resolves somewhere refused gets nowhere', async () => {
    // DNS rebinding: the first answer is an address the policy allows, the next is one it doesn't.
    // The resolver is stubbed, so the test doesn't depend on the machine's.
    const answers = ['127.0.0.1', '10.0.0.5']
    const asked: string[] = []
    vi.spyOn(dns, 'lookup').mockImplementation(((host: string, _options: unknown, callback: unknown) => {
      asked.push(host)
      const address = answers[asked.length - 1] ?? '10.0.0.5'
      ;(callback as (err: null, addresses: { address: string; family: number }[]) => void)(null, [
        { address, family: 4 },
      ])
    }) as typeof dns.lookup)

    const heard: EgressRefused[] = []
    const fetcher = egressFetch(parsePolicy(['127.0.0.1'], 'egress'))((r) => heard.push(r))
    const url = `http://rebind.example.test:${port}/first`

    const first = await fetcher(url)
    expect(first.status).toBe(200)
    expect(await first.text()).toBe('hello')
    expect(reached).toEqual(['/first'])

    await expect(fetcher(`http://rebind.example.test:${port}/second`)).rejects.toThrow()
    expect(asked).toEqual(['rebind.example.test', 'rebind.example.test'])
    expect(heard).toHaveLength(1)
    expect(heard[0]?.message).toBe(`${REFUSED}: rebind.example.test resolves to 10.0.0.5, not on the allowlist`)
    // The refused connection never reached the server.
    expect(reached).toEqual(['/first'])
  })

  it('connects to an address a prefix allows', async () => {
    const fetcher = egressFetch(parsePolicy(['127.0.0.0/8'], 'egress'))(() => {
      throw new Error('nothing should be refused')
    })
    const response = await fetcher(`http://127.0.0.1:${port}/`)
    expect(response.status).toBe(200)
    expect(await response.text()).toBe('hello')
  })

  it('connects to a host the policy names, whatever it resolves to', async () => {
    const fetcher = egressFetch(parsePolicy(['localhost'], 'egress'))(() => {
      throw new Error('nothing should be refused')
    })
    const response = await fetcher(`http://localhost:${port}/`)
    expect(response.status).toBe(200)
  })

  it('does not follow a redirect: it is the answer', async () => {
    const fetcher = egressFetch(parsePolicy(['127.0.0.0/8'], 'egress'))(() => {})
    const response = await fetcher(`http://127.0.0.1:${port}/redirect`)
    expect(response.status).toBe(302)
    expect(response.headers.get('location')).toBe('http://127.0.0.1:1/elsewhere')
  })
})
