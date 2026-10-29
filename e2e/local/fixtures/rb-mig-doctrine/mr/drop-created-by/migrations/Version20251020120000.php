<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20251020120000 extends AbstractMigration
{
    public function getDescription(): string
    {
        return 'Drop orders.created_by';
    }

    public function up(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders DROP COLUMN created_by');
    }

    public function down(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders ADD created_by VARCHAR(180) DEFAULT NULL');
    }
}
