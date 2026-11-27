import { readFileSync } from 'node:fs'
import { relative } from 'node:path'

const files = process.argv.slice(2)
if (files.length === 0) {
  console.error('usage: vitest-report <report.json>...')
  process.exit(2)
}

let failures = 0
const details = []
for (const file of files) {
  let report
  try {
    report = JSON.parse(readFileSync(file, 'utf8'))
  } catch (e) {
    failures++
    console.log(`????\t${file}\t(unreadable: ${e.message})`)
    continue
  }
  for (const result of report.testResults ?? []) {
    const name = relative(process.cwd(), result.name)
    const seconds = ((result.endTime - result.startTime) / 1000).toFixed(3)
    const failed = (result.assertionResults ?? []).filter((a) => a.status === 'failed')
    if (result.status === 'passed') {
      console.log(`ok  \t${name}\t${seconds}s`)
      continue
    }
    console.log(`FAIL\t${name}\t${seconds}s`)
    for (const a of failed) {
      failures++
      const title = [...(a.ancestorTitles ?? []), a.title].join(' > ')
      const errors = (a.failureMessages ?? []).join('\n')
      details.push(`\n===== FAIL: ${name} > ${title} (${Math.round(a.duration ?? 0)} ms) =====\n${errors}`)
    }
    if (failed.length === 0) {
      failures++
      details.push(`\n===== ${name}: failed outside a test =====\n${result.message || '(no message)'}`)
    } else if (result.message) {
      details.push(`\n===== ${name}: the file's own message =====\n${result.message}`)
    }
  }
  if (report.success === false && (report.numFailedTests ?? 0) === 0 && (report.numFailedTestSuites ?? 0) === 0) {
    failures++
    details.push(`\n===== ${file}: the run failed with no failing test: see the errors in the step above =====`)
  }
}
for (const d of details) console.log(d)
if (failures === 0) console.log('\nno failing test found in the reports')
