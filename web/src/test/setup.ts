// Runs before every test file (vite.config.ts, test.setupFiles).

import { configure } from '@testing-library/react'

// How long findBy… and waitFor wait for what the page ends up showing. Testing Library's
// default, 1 s, is tight on a CI runner shared with other jobs: there, a language switch that
// takes about 20 ms on an idle machine (the whole app drawn again in the new language) took
// just over 1 s once, and the test failed with the page about to be right. The tests check
// what the page shows, not how fast it gets there. 4 s stays under the 10 s a whole test has
// (vite.config.ts), so a page that never gets there still fails with Testing Library's own
// message and the page it saw.
configure({ asyncUtilTimeout: 4000 })
