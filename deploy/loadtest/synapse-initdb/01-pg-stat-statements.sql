-- pg_stat_statements для отчета loadgen: библиотека загружается
-- через shared_preload_libraries (command в docker-compose.synapse.yml).
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
