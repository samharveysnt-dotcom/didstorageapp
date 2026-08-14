-- Additional DID-provisioning countries — extends the v1 seed set (0002)
-- with the next tier of commonly-hosted wholesale-DID markets.
--
-- Selection rules:
--   - only countries with a working ITU-T country code AND at least one
--     mainstream DID wholesale supplier per public reference (Twilio,
--     Bandwidth, DIDWW, DIDLogic, VoIP.ms, Peerless Network, Skynet Telecom).
--   - Chinese special-administrative regions (Macau) included; mainland
--     China excluded — most wholesalers don't offer general DID delivery
--     into +86 without an ICP licence, and adding the ISO with no working
--     supplier would let admins provision numbers that never route.
--   - Kosovo skipped for the same reason (no ITU-assigned country code).
--
-- ON CONFLICT DO NOTHING is idempotent: re-running the migration on a
-- box where an admin manually inserted one of these ISOs already just
-- keeps the existing row's name intact.

INSERT INTO countries (iso, name) VALUES
  ('KR','South Korea'),
  ('TW','Taiwan'),
  ('VN','Vietnam'),
  ('PK','Pakistan'),
  ('BD','Bangladesh'),
  ('LK','Sri Lanka'),
  ('SA','Saudi Arabia'),
  ('QA','Qatar'),
  ('KW','Kuwait'),
  ('BH','Bahrain'),
  ('OM','Oman'),
  ('JO','Jordan'),
  ('LB','Lebanon'),
  ('EG','Egypt'),
  ('MA','Morocco'),
  ('TN','Tunisia'),
  ('DZ','Algeria'),
  ('NG','Nigeria'),
  ('KE','Kenya'),
  ('GH','Ghana'),
  ('CI','Cote dIvoire'),
  ('SN','Senegal'),
  ('UG','Uganda'),
  ('TZ','Tanzania'),
  ('RW','Rwanda'),
  ('CL','Chile'),
  ('CO','Colombia'),
  ('PE','Peru'),
  ('VE','Venezuela'),
  ('EC','Ecuador'),
  ('UY','Uruguay'),
  ('PY','Paraguay'),
  ('BO','Bolivia'),
  ('DO','Dominican Republic'),
  ('CR','Costa Rica'),
  ('PA','Panama'),
  ('GT','Guatemala'),
  ('SV','El Salvador'),
  ('HN','Honduras'),
  ('JM','Jamaica'),
  ('TT','Trinidad and Tobago'),
  ('PR','Puerto Rico'),
  ('GR','Greece'),
  ('RO','Romania'),
  ('BG','Bulgaria'),
  ('HU','Hungary'),
  ('SK','Slovakia'),
  ('SI','Slovenia'),
  ('HR','Croatia'),
  ('RS','Serbia'),
  ('BA','Bosnia and Herzegovina'),
  ('MK','North Macedonia'),
  ('AL','Albania'),
  ('MT','Malta'),
  ('CY','Cyprus'),
  ('LU','Luxembourg'),
  ('IS','Iceland'),
  ('EE','Estonia'),
  ('LV','Latvia'),
  ('LT','Lithuania'),
  ('UA','Ukraine'),
  ('MD','Moldova'),
  ('GE','Georgia'),
  ('AM','Armenia'),
  ('AZ','Azerbaijan'),
  ('KZ','Kazakhstan'),
  ('UZ','Uzbekistan'),
  ('TR','Turkey'),
  ('RU','Russia'),
  ('BY','Belarus'),
  ('MO','Macau')
ON CONFLICT (iso) DO NOTHING;
