-- Rollback the additional-countries seed. Only removes rows we added
-- AND only when no DIDs / rate cards reference them (FK RESTRICT would
-- error otherwise, which is the correct behaviour — an admin has DIDs
-- provisioned in that country and doesn't want us silently dropping the
-- catalogue entry).

DELETE FROM countries
 WHERE iso IN (
   'KR','TW','VN','PK','BD','LK','SA','QA','KW','BH','OM','JO','LB',
   'EG','MA','TN','DZ','NG','KE','GH','CI','SN','UG','TZ','RW',
   'CL','CO','PE','VE','EC','UY','PY','BO','DO','CR','PA','GT','SV','HN',
   'JM','TT','PR',
   'GR','RO','BG','HU','SK','SI','HR','RS','BA','MK','AL','MT','CY','LU','IS',
   'EE','LV','LT','UA','MD','GE','AM','AZ','KZ','UZ','TR','RU','BY','MO'
 )
 AND NOT EXISTS (SELECT 1 FROM dids       WHERE country_iso = countries.iso)
 AND NOT EXISTS (SELECT 1 FROM rate_cards WHERE country_iso = countries.iso);
