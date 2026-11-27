import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Input, type InputProps } from '../ui/Input'

export function PasswordInput(props: Omit<InputProps, 'type'>) {
  const t = useT()
  const [shown, setShown] = useState(false)
  return (
    <div className="ui-auth-password">
      <Input {...props} type={shown ? 'text' : 'password'} spellCheck={false} autoCapitalize="none" autoCorrect="off" />
      <Button
        variant="ghost"
        size="sm"
        aria-label={shown ? t('auth.hidePassword') : t('auth.showPassword')}
        disabled={props.disabled}
        onClick={() => setShown((s) => !s)}
      >
        {shown ? t('auth.hide') : t('auth.show')}
      </Button>
    </div>
  )
}
