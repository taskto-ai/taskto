// A password field's control, with a Show button inside its edge that shows what was typed, for
// checking it on a phone's keyboard. Its name holds the word it shows ("Show" is "Show password",
// "Hide" is "Hide password"), so a person who speaks to their device can say what they see. The
// password is shown only while the person asks for it, and never leaves the field. Inside a
// Field, as an Input.

import { useState } from 'react'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Input, type InputProps } from '../ui/Input'

export function PasswordInput(props: Omit<InputProps, 'type'>) {
  const t = useT()
  const [shown, setShown] = useState(false)
  return (
    <div className="ui-auth-password">
      <Input
        {...props}
        type={shown ? 'text' : 'password'}
        // Not checked or corrected as words while it is shown.
        spellCheck={false}
        autoCapitalize="none"
        autoCorrect="off"
      />
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
