CREATE TABLE slider_captcha_challenges (
    id UUID PRIMARY KEY,
    mobile_e164 TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('REGISTER', 'LOGIN')),
    target_x INTEGER NOT NULL CHECK (target_x >= 0),
    status TEXT NOT NULL CHECK (status IN ('CREATED', 'PASSED', 'FAILED', 'EXPIRED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
    expires_at TIMESTAMPTZ NOT NULL,
    passed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX slider_captcha_expiry_idx
    ON slider_captcha_challenges (expires_at)
    WHERE status = 'CREATED';

CREATE INDEX slider_captcha_mobile_idx
    ON slider_captcha_challenges (mobile_e164, purpose, created_at DESC);
