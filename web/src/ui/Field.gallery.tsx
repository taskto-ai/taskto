import { useT } from '../i18n'
import { Field } from './Field'
import { SpecimenGrid } from './GalleryParts'
import { Input } from './Input'
import { Select } from './Select'
import { Textarea } from './Textarea'

export const title = 'Field · Input · Select · Textarea'

export default function FieldGallery() {
  const t = useT()
  return (
    <SpecimenGrid>
      <Field label={t('gallery.sample.workspace')} required>
        <Input defaultValue={t('gallery.sample.workspaceValue')} />
      </Field>
      <Field
        label={t('gallery.sample.email')}
        hint={t('gallery.sample.emailHint')}
        error={t('gallery.sample.emailError')}
      >
        <Input type="email" defaultValue={t('gallery.sample.emailValue')} />
      </Field>
      <Field label={t('gallery.sample.role')}>
        <Select defaultValue="member">
          <option value="member">{t('gallery.sample.member')}</option>
          <option value="admin">{t('gallery.sample.admin')}</option>
        </Select>
      </Field>
      <Field label={t('gallery.sample.notes')} hint={t('gallery.sample.notesHint')}>
        <Textarea placeholder={t('gallery.sample.notesPlaceholder')} />
      </Field>
      <Field label={t('gallery.state.disabled')}>
        <Input disabled defaultValue={t('gallery.sample.workspaceValue')} />
      </Field>
      <Field label={t('gallery.state.disabled')}>
        <Select disabled defaultValue="member">
          <option value="member">{t('gallery.sample.member')}</option>
        </Select>
      </Field>
      <Field label={t('gallery.sample.locked')} hint={t('gallery.state.readOnly')}>
        <Input readOnly defaultValue={t('gallery.sample.lockedValue')} />
      </Field>
      <Field label={t('gallery.state.small')}>
        <Input size="sm" placeholder={t('gallery.sample.search')} />
      </Field>
    </SpecimenGrid>
  )
}
