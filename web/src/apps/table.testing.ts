import { APPS_CATALOG_ID } from './catalog'

export const tableColumns = [
  { field: 'title', label: 'Title', type: 'string' },
  { field: 'category', label: 'Category', type: 'choice' },
  { field: 'due_date', label: 'Due', type: 'date' },
  { field: 'amount', label: 'Amount', type: 'number' },
  { field: 'done', label: 'Done', type: 'boolean' },
]

export const tableItems = [
  {
    id: 1,
    _revision: 4,
    _labels: { category: 'Home' },
    title: 'Pay rent',
    category: 'home',
    due_date: '2026-03-08',
    amount: 1234.5,
    done: false,
  },
  {
    id: 2,
    _revision: 1,
    _labels: { category: 'Other' },
    title: 'Fix the shelf',
    category: 'other',
    due_date: '2026-01-02',
    amount: 20,
    done: true,
  },
  { id: 3, _revision: 2, _labels: { category: 'Work' }, title: 'Call the bank', category: 'work', amount: 3 },
]

export function tableSurface(items: unknown[] = tableItems, table: Record<string, unknown> = {}) {
  return [
    { version: 'v0.9', createSurface: { surfaceId: 'app-1', catalogId: APPS_CATALOG_ID } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'app-1',
        components: [
          {
            id: 'root',
            component: 'Table',
            items: { path: '/items' },
            label: 'Tasks',
            columns: tableColumns,
            open: 'open_task',
            ...table,
          },
        ],
      },
    },
    { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items, draft: {} } } },
  ]
}
