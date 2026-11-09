-- Accounts whose ONLY password verifier is 10g, 11g or 12c, for
-- driver/oracle/verifier_live_test.go. Run as SYSDBA in the PDB by
-- scripts/setup_verifier_users.sh, which also lowers
-- SQLNET.ALLOWED_LOGON_VERSION_SERVER so those verifiers can log in.
--
-- Each account is created with IDENTIFIED BY (the server computes all its
-- verifiers), then reset with IDENTIFIED BY VALUES to the one verifier under
-- test, read back from SYS.USER$. The 10g one needs a release that still
-- accepts 10g VALUES: Oracle 21c does; 23ai and 26ai answer ORA-02153.
whenever sqlerror exit failure
set feedback off heading off pagesize 0 linesize 200 verify off
begin
  for u in (select username from dba_users where username in ('ADBC_TEST_10G', 'ADBC_TEST_11G', 'ADBC_TEST_12C')) loop
    execute immediate 'drop user ' || u.username || ' cascade';
  end loop;
end;
/
create user adbc_test_10g identified by Tiger10g;
create user adbc_test_11g identified by Tiger11g;
create user adbc_test_12c identified by Tiger12c;
grant create session to adbc_test_10g, adbc_test_11g, adbc_test_12c;
declare
  v varchar2(4000);
begin
  select password into v from sys.user$ where name = 'ADBC_TEST_10G';
  execute immediate 'alter user adbc_test_10g identified by values ''' || v || '''';
  select regexp_substr(spare4, 'S:[0-9A-F]{60}') into v from sys.user$ where name = 'ADBC_TEST_11G';
  execute immediate 'alter user adbc_test_11g identified by values ''' || v || '''';
  select regexp_substr(spare4, 'T:[0-9A-F]{160}') into v from sys.user$ where name = 'ADBC_TEST_12C';
  execute immediate 'alter user adbc_test_12c identified by values ''' || v || '''';
end;
/
declare
  n number;
begin
  select count(*) into n from dba_users
   where (username, trim(password_versions)) in
         (('ADBC_TEST_10G', '10G'), ('ADBC_TEST_11G', '11G'), ('ADBC_TEST_12C', '12C'));
  if n != 3 then
    raise_application_error(-20001, 'only ' || n || ' of 3 single-verifier accounts were set up');
  end if;
end;
/
select username || ': ' || password_versions from dba_users where username like 'ADBC\_TEST\_1%' escape '\' order by 1;
exit
