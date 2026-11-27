import { randomBytes } from 'node:crypto'
import { createServer } from 'node:http'

const code = process.env.ACCESS_CODE
const user = process.env.LOGIN_USER
const password = process.env.LOGIN_PASSWORD
if (!code || !user || !password) throw new Error('ACCESS_CODE, LOGIN_USER and LOGIN_PASSWORD are required')
const session = randomBytes(16).toString('hex')

const style = `
body{margin:0;font-family:sans-serif}
h1{position:fixed;top:0;left:5vw;margin:0;font-size:2vh}
input{position:fixed;left:5vw;width:90vw;height:34vh;font-size:5vh;box-sizing:border-box}
#username{top:4vh} #password{top:46vh}
button{position:fixed;left:5vw;top:84vh;height:8vh;font-size:4vh}
p.err{position:fixed;top:92vh;left:5vw;color:#b00;margin:0}
`

const loginPage = (error) => `<!doctype html><html><head><meta charset="utf-8">
<title>Example Portal: sign in</title><style>${style}</style></head>
<body><h1>Example Portal: sign in to see your dashboard</h1>
<form method="post" action="/login">
<input id="username" name="username" placeholder="Username" autocomplete="off" aria-label="Username">
<input id="password" name="password" type="password" placeholder="Password" aria-label="Password">
<button type="submit">Sign in</button></form>${error ? `<p class="err">${error}</p>` : ''}</body></html>`

const dashboard = `<!doctype html><html><head><meta charset="utf-8"><title>Example Portal: dashboard</title></head>
<body><h1>Dashboard</h1><p>Signed in as ${user}.</p><p>Your access code is <strong>${code}</strong>.</p></body></html>`

const send = (res, status, body = '', headers = {}) => {
  res.writeHead(status, { 'Content-Type': 'text/html; charset=utf-8', ...headers })
  res.end(body)
}

createServer((req, res) => {
  const signedIn = (req.headers.cookie ?? '').split(/;\s*/).includes(`session=${session}`)
  const path = new URL(req.url ?? '/', 'http://localhost').pathname
  if (req.method === 'GET' && path === '/dashboard') {
    return signedIn ? send(res, 200, dashboard) : send(res, 303, '', { Location: '/' })
  }
  if (req.method === 'GET' && (path === '/' || path === '/login')) {
    return signedIn ? send(res, 303, '', { Location: '/dashboard' }) : send(res, 200, loginPage())
  }
  if (req.method === 'POST' && path === '/login') {
    let body = ''
    req.setEncoding('utf8')
    req.on('data', (b) => {
      body += b
    })
    req.on('end', () => {
      const form = new URLSearchParams(body)
      const ok = form.get('username') === user && form.get('password') === password
      console.error(`login attempt ok=${ok}`)
      if (ok)
        return send(res, 303, '', { Location: '/dashboard', 'Set-Cookie': `session=${session}; Path=/; HttpOnly` })
      send(res, 200, loginPage('Wrong username or password.'))
    })
    return
  }
  send(res, 404, 'not found')
}).listen(8000, '127.0.0.1', () => console.error('fakelogin listening on 127.0.0.1:8000'))
