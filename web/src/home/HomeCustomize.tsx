// Home's customize mode, under the widgets: the pins in the menu, the space's defaults the person
// hid, and the person's apps, each with the widgets and the pin it offers, to add or take off. Who
// may publish an app of a shared space also sets what it offers as the space's defaults, from the
// row's "more" menu. Visible labels are short ("Add", "Pin"); each control's name says what it acts
// on ("Add Spending to Home"), so a list of them heard one after another is still clear.

import { useEffect, useRef } from 'react'
import { type AvailableApp, api, type HomeView, ws } from '../api'
import { type Key, useT } from '../i18n'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { MenuItem, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { type Act, focusName, type Move, neighbours, type Run } from './HomePage'
import { ChecklistRestore } from './SetupChecklist'
import { useHomeLoad } from './store'
import { itemKey } from './WidgetCard'

export function HomeCustomize({
  id,
  workspace,
  admin,
  home,
  run,
  move,
  remove,
  hide,
  focusOnOpen = false,
}: {
  id: string
  workspace: number
  admin: boolean
  home: HomeView
  run: Run
  move: Move
  remove: Act
  hide: Act
  // Put the keyboard on the "Add from your apps" heading when this opens.
  focusOnOpen?: boolean
}) {
  const t = useT()
  const addHeading = useRef<HTMLHeadingElement>(null)
  // biome-ignore lint/correctness/useExhaustiveDependencies: only when it opens
  useEffect(() => {
    if (focusOnOpen) addHeading.current?.focus()
  }, [])
  const base = ws(workspace)
  const available = useHomeLoad(
    () => api<{ apps: AvailableApp[] }>(`${base}/home/available`).then((r) => r.apps),
    [base],
  )
  // An offer's controls: the add or remove button, and its "more" menu.
  const offer = (a: AvailableApp, what: string, control: string) =>
    focusName('offer', `${a.space_id}:${a.app_id}:${what}`, control)
  const ownPins = home.pins.filter((p) => p.source === 'personal')
  const place = (a: AvailableApp, kind: 'widget' | 'pin', name: string, widget?: string) =>
    run(
      () =>
        api(`${base}/home/items`, { method: 'POST', body: { kind, space_id: a.space_id, app_id: a.app_id, widget } }),
      {
        message: t(kind === 'pin' ? 'home.announce.pinned' : 'home.announce.added', { name }),
        focus: [offer(a, widget ?? 'pin', 'toggle')],
      },
    )
  const setDefault = (a: AvailableApp, kind: 'widget' | 'pin', name: string, widget?: string) =>
    run(
      () =>
        api(`${base}/spaces/${a.space_id}/home/defaults`, {
          method: 'POST',
          body: { kind, app_id: a.app_id, widget },
        }),
      // The menu gives the keyboard back to its button, which stays.
      { message: t('home.announce.defaultSet', { name }) },
    )
  const unsetDefault = (a: AvailableApp, id: number, name: string) =>
    run(() => api(`${base}/spaces/${a.space_id}/home/defaults/${id}`, { method: 'DELETE' }), {
      message: t('home.announce.defaultUnset', { name }),
    })
  const space = (a: AvailableApp) => (a.space_kind === 'private' ? t('knowledge.spaces.private') : a.space_name)
  const placedWidget = (a: AvailableApp, id: string) =>
    home.widgets.find((i) => i.source === 'personal' && i.app_id === a.app_id && i.widget_id === id)
  const placedPin = (a: AvailableApp) => home.pins.find((i) => i.source === 'personal' && i.app_id === a.app_id)

  // The "more" menu of an offered widget or pin, for those who may set the space's defaults.
  // Its name says whether it is the widget's or the pin's, which may share a name.
  const defaults = (
    a: AvailableApp,
    name: string,
    what: string,
    def: { id: number } | undefined,
    set: () => Promise<void>,
  ) =>
    a.can_set_defaults && (
      <MoreMenu label={t(what === 'pin' ? 'home.moreForPin' : 'home.moreForWidget', { name })}>
        {def ? (
          <MenuItem icon="close" onSelect={() => unsetDefault(a, def.id, name)}>
            {t('home.unsetDefaultNamed', { name })}
          </MenuItem>
        ) : (
          <MenuItem icon="pin" onSelect={set}>
            {t('home.setDefaultNamed', { name })}
          </MenuItem>
        )}
      </MoreMenu>
    )

  return (
    <div id={id} className="ui-home-customize">
      <p className="ui-home-help">{t('home.intro')}</p>
      {admin && <ChecklistRestore workspace={workspace} />}

      <section className="ui-home-section" aria-labelledby="ui-home-pins">
        <header className="ui-home-section-head">
          <Heading level={2} id="ui-home-pins">
            {t('home.pins')}
          </Heading>
          <p className="ui-home-help">{t('home.pinsIntro')}</p>
        </header>
        {home.pins.length === 0 ? (
          <p className="ui-home-help">{t('home.noPins')}</p>
        ) : (
          <Panel>
            <Rows label={t('home.pins')}>
              {home.pins.map((p) => {
                const name = p.label || p.app_name
                const at = ownPins.findIndex((i) => i.id === p.id)
                return (
                  <Row
                    key={itemKey(p)}
                    title={name}
                    status={
                      <>
                        {p.state === 'unavailable' && (
                          <Badge tone="warning">{t(`home.pinState.${p.reason ?? 'removed'}` as Key)}</Badge>
                        )}
                        {p.source === 'default' && <Badge>{t('home.defaultBadge')}</Badge>}
                      </>
                    }
                    actions={
                      p.source === 'personal' ? (
                        <>
                          <IconButton
                            size="sm"
                            icon="up"
                            label={t('home.moveEarlier', { name })}
                            disabled={at <= 0}
                            data-home-focus={focusName('pin', itemKey(p), 'earlier')}
                            onClick={() => move('pin', p, -1)}
                          />
                          <IconButton
                            size="sm"
                            icon="down"
                            label={t('home.moveLater', { name })}
                            disabled={at === ownPins.length - 1}
                            data-home-focus={focusName('pin', itemKey(p), 'later')}
                            onClick={() => move('pin', p, 1)}
                          />
                          <IconButton
                            size="sm"
                            icon="close"
                            label={t('home.removePinNamed', { name })}
                            data-home-focus={focusName('pin', itemKey(p), 'remove')}
                            onClick={() => remove(p, name)}
                          />
                        </>
                      ) : (
                        <IconButton
                          size="sm"
                          icon="close"
                          label={t('home.hideNamed', { name })}
                          data-home-focus={focusName('pin', itemKey(p), 'hide')}
                          onClick={() => hide(p, name)}
                        />
                      )
                    }
                  />
                )
              })}
            </Rows>
          </Panel>
        )}
      </section>

      {home.hidden.length > 0 && (
        <section className="ui-home-section" aria-labelledby="ui-home-hidden">
          <Heading level={2} id="ui-home-hidden">
            {t('home.hidden')}
          </Heading>
          <Panel>
            <Rows label={t('home.hidden')}>
              {home.hidden.map((h) => {
                const name = h.widget?.title || h.label || h.app_name
                return (
                  <Row
                    key={itemKey(h)}
                    title={name}
                    detail={t('home.fromApp', { name: h.app_name })}
                    actions={
                      <Button
                        size="sm"
                        aria-label={t('home.showNamed', { name })}
                        data-home-focus={focusName('hidden', itemKey(h), 'show')}
                        onClick={() =>
                          run(() => api(`${base}/home/hidden/${h.default_id}`, { method: 'DELETE' }), {
                            message: t('home.announce.shown', { name }),
                            focus: neighbours('hidden', home.hidden, h).map((n) =>
                              n.replace(/:(remove|hide)$/, ':show'),
                            ),
                          })
                        }
                      >
                        {t('home.show')}
                      </Button>
                    }
                  />
                )
              })}
            </Rows>
          </Panel>
        </section>
      )}

      <section className="ui-home-section" aria-labelledby="ui-home-add">
        <Heading level={2} id="ui-home-add" ref={addHeading} tabIndex={-1}>
          {t('home.add')}
        </Heading>
        {available.error && <Notice tone="danger">{available.error}</Notice>}
        {available.data?.length === 0 && <p className="ui-home-help">{t('home.noApps')}</p>}
        {available.data?.map((a) => {
          const pin = placedPin(a)
          const pinDefault = a.defaults.find((d) => d.kind === 'pin')
          const pinName = a.pin?.label || a.name
          return (
            <div key={`${a.space_id}:${a.app_id}`} className="ui-home-group">
              <Heading level={3} size="subsection">
                {a.name} <span className="ui-home-group-space">{space(a)}</span>
              </Heading>
              <Panel>
                <Rows label={a.name}>
                  {a.widgets.map((w) => {
                    const placed = placedWidget(a, w.id)
                    const def = a.defaults.find((d) => d.kind === 'widget' && d.widget === w.id)
                    return (
                      <Row
                        key={w.id}
                        title={w.title}
                        detail={t(`home.kind.${w.kind}` as Key)}
                        status={
                          <>
                            {placed && (
                              <Badge tone="success" dot>
                                {t('home.added')}
                              </Badge>
                            )}
                            {def && <Badge>{t('home.defaultBadge')}</Badge>}
                          </>
                        }
                        actions={
                          <>
                            {placed ? (
                              <Button
                                size="sm"
                                aria-label={t('home.removeNamed', { name: w.title })}
                                data-home-focus={offer(a, w.id, 'toggle')}
                                onClick={() => remove(placed, w.title, [offer(a, w.id, 'toggle')])}
                              >
                                {t('home.remove')}
                              </Button>
                            ) : (
                              <Button
                                size="sm"
                                icon="plus"
                                aria-label={t('home.addNamed', { name: w.title })}
                                data-home-focus={offer(a, w.id, 'toggle')}
                                onClick={() => place(a, 'widget', w.title, w.id)}
                              >
                                {t('home.addShort')}
                              </Button>
                            )}
                            {defaults(a, w.title, w.id, def, () => setDefault(a, 'widget', w.title, w.id))}
                          </>
                        }
                      />
                    )
                  })}
                  <Row
                    title={pinName}
                    detail={t('home.kind.pin')}
                    status={
                      <>
                        {pin && (
                          <Badge tone="success" dot>
                            {t('home.pinned')}
                          </Badge>
                        )}
                        {pinDefault && <Badge>{t('home.defaultBadge')}</Badge>}
                      </>
                    }
                    actions={
                      <>
                        {pin ? (
                          <Button
                            size="sm"
                            aria-label={t('home.unpinNamed', { name: pinName })}
                            data-home-focus={offer(a, 'pin', 'toggle')}
                            onClick={() => remove(pin, pinName, [offer(a, 'pin', 'toggle')])}
                          >
                            {t('home.unpin')}
                          </Button>
                        ) : (
                          <Button
                            size="sm"
                            icon="pin"
                            aria-label={t('home.pinNamed', { name: pinName })}
                            data-home-focus={offer(a, 'pin', 'toggle')}
                            onClick={() => place(a, 'pin', pinName)}
                          >
                            {t('home.pin')}
                          </Button>
                        )}
                        {defaults(a, pinName, 'pin', pinDefault, () => setDefault(a, 'pin', pinName))}
                      </>
                    }
                  />
                </Rows>
              </Panel>
            </div>
          )
        })}
        {available.data?.some((a) => a.can_set_defaults) && <p className="ui-home-help">{t('home.defaultsNote')}</p>}
      </section>
    </div>
  )
}
